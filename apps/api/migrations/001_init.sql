-- =============================================================================
-- Safar Share — migrations/001_init.sql
-- PostgreSQL 16 + PostGIS 3.4.  Apply with:
--   psql "$DATABASE_URL" -f migrations/001_init.sql
-- Idempotent: safe to run more than once.
-- =============================================================================

CREATE EXTENSION IF NOT EXISTS postgis;

-- -----------------------------------------------------------------------------
-- tenant — B2B office pools; discount_pct is the membership/tenant discount
-- (§5.3): must stay inside the mandated 10–20% band.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenant (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        NOT NULL,
    gstin         text,
    domain        text,                                  -- e.g. acme.com (employee email domain)
    discount_pct  numeric(5,4) NOT NULL DEFAULT 0.10
                  CHECK (discount_pct >= 0.10 AND discount_pct <= 0.20),
    policy        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- -----------------------------------------------------------------------------
-- account — OTP-authenticated users (phone stored encrypted, keyed by salted hash)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS account (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_hash    text        NOT NULL UNIQUE,            -- HMAC-SHA256(pepper, E.164) hex
    phone_enc     bytea       NOT NULL,                   -- AES-GCM ciphertext (DPDP at-rest)
    full_name     text        NOT NULL DEFAULT '',
    email         text,
    role          text        NOT NULL DEFAULT 'rider'
                  CHECK (role IN ('rider','driver','both','admin')),
    tenant_id     uuid REFERENCES tenant(id),
    age_confirmed boolean     NOT NULL DEFAULT false,     -- 18+ age gate (DPDP)
    ban_status    text        NOT NULL DEFAULT 'active'
                  CHECK (ban_status IN ('active','suspended','banned')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- -----------------------------------------------------------------------------
-- vehicle — driver-owned vehicles; is_private is the §1.15 pooled-vs-rental flag
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS vehicle (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        uuid NOT NULL REFERENCES account(id),
    make_model      text NOT NULL,
    reg_number      text NOT NULL,
    is_private      boolean NOT NULL DEFAULT true,
    seats_available int NOT NULL CHECK (seats_available BETWEEN 2 AND 12),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_vehicle_owner ON vehicle(owner_id);

-- -----------------------------------------------------------------------------
-- route — driver's published corridor.  waypoints_line is the PostGIS corridor
-- used for matching (§2: GIST geography now; H3 cell table is the later upgrade).
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS route (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    driver_id       uuid NOT NULL REFERENCES account(id),
    vehicle_id      uuid NOT NULL REFERENCES vehicle(id),
    origin_label    text NOT NULL,
    dest_label      text NOT NULL,
    waypoints_line  geography(LineString,4326) NOT NULL,
    departure_time  time NOT NULL,                       -- local time-of-day
    weekday_mask    int  NOT NULL DEFAULT 127            -- bit0=Mon .. bit6=Sun
                    CHECK (weekday_mask BETWEEN 0 AND 127),
    duration_min    int  NOT NULL CHECK (duration_min > 0),
    distance_km     numeric(6,2) NOT NULL CHECK (distance_km > 0),
    seats           int NOT NULL CHECK (seats BETWEEN 1 AND 11),
    price_estimate  numeric(10,2) NOT NULL CHECK (price_estimate >= 0),
    status          text NOT NULL DEFAULT 'PUBLISHED'
                    CHECK (status IN ('DRAFT','PUBLISHED','CANCELLED','COMPLETED')),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_route_waypoints ON route USING GIST (waypoints_line);
CREATE INDEX IF NOT EXISTS idx_route_driver    ON route(driver_id);
CREATE INDEX IF NOT EXISTS idx_route_status    ON route(status);

-- -----------------------------------------------------------------------------
-- route_cell — H3 corridor index.  Populated alongside route publish so the
-- later H3-based matcher can take over without a schema change (§2).
-- h3_index stores resolution-9 cell ids as hex strings.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS route_cell (
    route_id  uuid NOT NULL REFERENCES route(id) ON DELETE CASCADE,
    h3_index  text NOT NULL,
    PRIMARY KEY (route_id, h3_index)
);

-- -----------------------------------------------------------------------------
-- ride_instance — one route on one calendar date; the unit that gets booked.
-- seats_booked is updated under SELECT ... FOR UPDATE inside the booking tx.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ride_instance (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    route_id     uuid NOT NULL REFERENCES route(id),
    driver_id    uuid NOT NULL REFERENCES account(id),
    ride_date    date NOT NULL,
    seats_booked int  NOT NULL DEFAULT 0 CHECK (seats_booked >= 0),
    status       text NOT NULL DEFAULT 'PUBLISHED'
                 CHECK (status IN ('DRAFT','PUBLISHED','CANCELLED','COMPLETED')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (route_id, ride_date)
);
CREATE INDEX IF NOT EXISTS idx_ride_search ON ride_instance(ride_date, status);
CREATE INDEX IF NOT EXISTS idx_ride_driver ON ride_instance(driver_id);

-- -----------------------------------------------------------------------------
-- booking — idempotent per (passenger, Idempotency-Key); state machine per §5.6:
--   PENDING_PAYMENT -> CONFIRMED | EXPIRED
--   CONFIRMED       -> CANCELLED | COMPLETED
--   CANCELLED / COMPLETED / EXPIRED are terminal.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS booking (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ride_instance_id uuid NOT NULL REFERENCES ride_instance(id),
    passenger_id    uuid NOT NULL REFERENCES account(id),
    seats           int  NOT NULL CHECK (seats >= 1),
    status          text NOT NULL DEFAULT 'PENDING_PAYMENT'
                    CHECK (status IN ('PENDING_PAYMENT','CONFIRMED','CANCELLED','COMPLETED','EXPIRED')),
    fare            numeric(10,2) NOT NULL CHECK (fare >= 0),
    idempotency_key text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (passenger_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_booking_ride    ON booking(ride_instance_id);
CREATE INDEX IF NOT EXISTS idx_booking_passenger ON booking(passenger_id);

-- -----------------------------------------------------------------------------
-- payment — one row per intent; webhook event_id dedupe guarantees retries are
-- safe (UNIQUE below).  status: CREATED -> CAPTURED | FAILED | REFUNDED.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payment (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id    uuid NOT NULL REFERENCES booking(id),
    amount        numeric(10,2) NOT NULL CHECK (amount >= 0),
    status        text NOT NULL DEFAULT 'CREATED'
                  CHECK (status IN ('CREATED','CAPTURED','FAILED','REFUNDED')),
    provider      text NOT NULL DEFAULT 'upi',
    provider_ref  text,                                   -- UPI txn id / PG reference
    upi_intent    text NOT NULL DEFAULT '',
    event_id      text UNIQUE,                            -- last webhook event applied
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_payment_booking ON payment(booking_id);

-- -----------------------------------------------------------------------------
-- ledger_entry — double-entry money trail.  Every transaction's debits must
-- equal its credits (enforced app-side before insert).
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ledger_entry (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id text NOT NULL,
    account        text NOT NULL,
    debit          numeric(10,2) NOT NULL DEFAULT 0 CHECK (debit  >= 0),
    credit         numeric(10,2) NOT NULL DEFAULT 0 CHECK (credit >= 0),
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (not (debit > 0 AND credit > 0))
);
CREATE INDEX IF NOT EXISTS idx_ledger_tx ON ledger_entry(transaction_id);

-- -----------------------------------------------------------------------------
-- rating — rider<->driver, 1..5, one per (from,to,ride)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS rating (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id       uuid NOT NULL REFERENCES booking(id),
    from_account_id  uuid NOT NULL REFERENCES account(id),
    to_account_id    uuid NOT NULL REFERENCES account(id),
    stars            int  NOT NULL CHECK (stars BETWEEN 1 AND 5),
    comment          text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (booking_id, from_account_id)
);

-- -----------------------------------------------------------------------------
-- verification — DigiLocker / KYC state machine (§10):
--   NOT_STARTED -> CONSENT_GIVEN -> DIGILOCKER_AUTH -> DOCS_FETCHED
--   DOCS_FETCHED -> AUTO_VERIFIED | MANUAL_REVIEW
--   AUTO_VERIFIED -> EXPIRED
-- PII is stored masked or encrypted only; raw aadhaar/DL numbers are never
-- persisted (only the DigiLocker doc reference + masked display fields).
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS verification (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id     uuid NOT NULL UNIQUE REFERENCES account(id),
    state          text NOT NULL DEFAULT 'NOT_STARTED'
                   CHECK (state IN ('NOT_STARTED','CONSENT_GIVEN','DIGILOCKER_AUTH',
                                    'DOCS_FETCHED','AUTO_VERIFIED','MANUAL_REVIEW','EXPIRED')),
    doc_type       text CHECK (doc_type IN ('DL','RC','AADHAAR')),
    doc_ref        text NOT NULL DEFAULT '',              -- DigiLocker URI, not the raw number
    masked_fields  jsonb NOT NULL DEFAULT '{}'::jsonb,    -- e.g. {"dl_last4":"1234","name":"..."}
    face_match_score numeric(4,3),
    consent_version text NOT NULL DEFAULT '',
    consent_at     timestamptz,
    expires_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- -----------------------------------------------------------------------------
-- outbox — transactional outbox (§5.8).  Poller claims rows with
-- FOR UPDATE SKIP LOCKED, applies exponential backoff + jitter, moves
-- attempts >= 5 to DLQ.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS outbox (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    topic           text NOT NULL,
    payload         jsonb NOT NULL,
    status          text NOT NULL DEFAULT 'PENDING'
                    CHECK (status IN ('PENDING','SENT','DLQ')),
    attempts        int  NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_outbox_due ON outbox(status, next_attempt_at)
    WHERE status = 'PENDING';
