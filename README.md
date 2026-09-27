# Safar Share

Carpooling MVP — riders share intercity rides at a fair, capped price. This
repository contains the full §0–§15 build: API, web app, Android app, iOS app,
infra, and CI.

## Architecture

A **Go 1.22 modular monolith** (chi + pgx + go-redis) with PostgreSQL/PostGIS
and Redis, plus an async **transactional outbox** for events (notifications,
ledger posts). Three clients talk to the same REST API:

| Component | Path | Stack |
|---|---|---|
| API | `apps/api/` | Go 1.22, chi, pgx/v5, go-redis/v9, jwt/v5 |
| Web | `apps/web/` | Next.js 14 (App Router), TypeScript, Tailwind |
| Android | `apps/mobile-android/` | Kotlin, Jetpack Compose, kotlinx.serialization |
| iOS | `apps/mobile-ios/` | SwiftUI (Xcode 15+, iOS 16+) |
| API contract | `docs/openapi.yaml` | OpenAPI 3.1 (16 operations) |
| Infra | `infra/terraform/` | ECS Fargate, RDS pg16, ElastiCache, S3+CloudFront |
| CI | `.github/workflows/ci.yml` | api / web / android / openapi jobs |

## Quickstart (API + dependencies)

```bash
docker compose up --build
# API        → http://localhost:8080
# Web        → http://localhost:3000
# PostGIS    → localhost:5432 (safar / safar)
# Redis      → localhost:6379
```

Environment is documented in `apps/api/.env.example`. Migration
`apps/api/internal/migrations/001_init.sql` runs automatically at boot.

### API surface (summary — authoritative spec is `docs/openapi.yaml`)

- `GET /healthz` — liveness + dependency degradation detail
- `POST /v1/auth/otp/request` · `POST /v1/auth/otp/verify` — phone OTP login
  (3 requests / 10 min / number, 18+ age gate, 24 h HS256 JWT with jti revocation)
- `POST /v1/routes` — publish a route with weekday mask + waypoints
- `POST /v1/rides/search` — PostGIS corridor search (2 km radius) ranked by
  0.40 detour/time + 0.25 cost + 0.20 rating + 0.15 reliability
- `POST /v1/bookings` — book seats; requires `Idempotency-Key` (8–128 chars),
  last seat → 409, advisory seat locks, payment intent + UPI intent returned
- `GET /v1/bookings` · `POST /v1/bookings/{id}/cancel`
- `POST /v1/payments/webhook` — HMAC-signed, ±300 s timestamp window,
  idempotent on `event_id`
- `POST /v1/verification/...` — DigiLocker KYC state machine
- Pricing engine: distance × rate, **cost-cap clamp is mandatory and wins**
  over min-fare, GST/fees, `pricing.Money()` rounding

## Running tests

```bash
# API unit tests + formatting gate
cd apps/api
gofmt -l .          # must print nothing
go vet ./...
go test -race ./...

# Integration tests (needs PostGIS + Redis, run in CI / docker-compose)
go test -race -tags integration ./...

# Web
cd apps/web && npm ci && npx next lint && npm run build

# Android (needs Android SDK; CI runs ./gradlew assembleDebug)
cd apps/mobile-android && ./gradlew assembleDebug

# iOS — open in Xcode
open apps/mobile-ios/SafarShare.xcodeproj
```

## Mobile notes

- **Android** targets API 26+. The debug build talks to the host machine's
  loopback via the emulator alias `10.0.2.2:8080` (`API_BASE` in
  `app/build.gradle.kts`). On a physical device set it to your LAN IP.
- **iOS** defaults to `127.0.0.1:8080` (`Api.baseURL` in `SafarShare/Api.swift`);
  change to your Mac's LAN IP for device runs.
- Both apps implement the same three MVP flows: OTP login, corridor search →
  book (UUID idempotency key per attempt), bookings list.

## Deploy

Terraform in `infra/terraform/` provisions the production stack (S3/DynamoDB
state, RDS, Redis, ECS Fargate, S3+CloudFront for the web app). CI builds and
pushes the API image; set `api_image` when applying. See
`infra/terraform/prod.tfvars.example` for variables.

## Repository layout

```
apps/api/            Go API (cmd/server, internal/modules/*, migrations)
apps/web/            Next.js 14 web app
apps/mobile-android/ Kotlin + Compose app (Gradle wrapper included)
apps/mobile-ios/     SwiftUI app (hand-written Xcode project)
docs/openapi.yaml    API contract
infra/terraform/     production IaC
.github/workflows/   CI (api, web, android, openapi jobs)
docker-compose.yml   local dev stack
```
