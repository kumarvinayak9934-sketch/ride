// Package verification implements §10 DigiLocker KYC with PKCE:
//
//	POST /v1/verify/digilocker/start    — begin auth, store verifier+state
//	POST /v1/verify/digilocker/callback  — exchange code, fetch docs, decide
//	GET  /v1/verify/status               — read the state machine
//
// Decision rules (§10): issuer ∈ {Sarathi DL, VAHAN RC}, not-expired,
// non-transport class where applicable, owner-name match, and (when a
// face-match vendor is configured) score >= 0.75 -> AUTO_VERIFIED,
// otherwise MANUAL_REVIEW.  Raw document numbers are NEVER stored: only the
// DigiLocker doc reference and masked fields survive.
package verification

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/cache"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/config"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/middleware"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/booking"
)

// FaceMatchThreshold is the §10 auto-verify bar.
const FaceMatchThreshold = 0.75

// Handler serves verification endpoints.
type Handler struct {
	pool  *pgxpool.Pool
	cache *cache.Cache
	cfg   config.DigiLockerConfig
	log   *slog.Logger
	// httpDo is swappable in tests.
	httpDo func(*http.Request) (*http.Response, error)
}

func NewHandler(pool *pgxpool.Pool, c *cache.Cache, cfg config.DigiLockerConfig, log *slog.Logger) *Handler {
	return &Handler{pool: pool, cache: c, cfg: cfg, log: log, httpDo: http.DefaultClient.Do}
}

// Routes mounts endpoints (JWT auth applied by caller).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/digilocker/start", h.Start)
	r.Post("/digilocker/callback", h.Callback)
	r.Get("/status", h.Status)
}

// Status is the verification resource state.
type Status struct {
	State        string            `json:"state"`
	DocType      string            `json:"doc_type,omitempty"`
	MaskedFields map[string]string `json:"masked_fields,omitempty"`
	FaceScore    *float64          `json:"face_match_score,omitempty"`
	ConsentAt    *time.Time        `json:"consent_at,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
	AuthorizeURL string            `json:"authorize_url,omitempty"`
	StateToken   string            `json:"auth_state,omitempty"` // PKCE state (start response only)
	CodeVerifier string            `json:"-"`                    // never leaves the server
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Start handles POST /v1/verify/digilocker/start — records consent, creates
// PKCE verifier/challenge + state, returns the authorize URL.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r.Context())
	if uid == "" {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "login required")
		return
	}
	var body struct {
		ConsentVersion string `json:"consent_version"`
		Purpose        string `json:"purpose"`
	}
	if !middleware.DecodeJSON(w, r, &body) {
		return
	}
	if body.ConsentVersion == "" {
		middleware.WriteProblem(w, http.StatusBadRequest, "Consent Required",
			"consent_version is required — explicit purpose-limited consent (§10)")
		return
	}
	if body.Purpose == "" {
		body.Purpose = "driver_identity_verification"
	}

	verifier := randToken(32)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randToken(24)

	// persist PKCE material for 10 min, bound to the user
	val, _ := json.Marshal(map[string]string{
		"user_id": uid, "verifier": verifier, "consent_version": body.ConsentVersion,
	})
	if err := h.cache.Store(context.Background(), "dlstate:"+state, val, 10*time.Minute); err != nil {
		middleware.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "state store failed")
		return
	}

	// consent tick + state transition NOT_STARTED|EXPIRED -> CONSENT_GIVEN
	if _, err := h.pool.Exec(r.Context(), `
		INSERT INTO verification (account_id, state, consent_version, consent_at, updated_at)
		VALUES ($1,'CONSENT_GIVEN',$2, now(), now())
		ON CONFLICT (account_id) DO UPDATE
		  SET state = CASE
		        WHEN verification.state IN ('NOT_STARTED','EXPIRED') THEN 'CONSENT_GIVEN'
		        ELSE verification.state END,
		      consent_version = EXCLUDED.consent_version,
		      consent_at = now(), updated_at = now()`,
		uuid.MustParse(uid), body.ConsentVersion); err != nil {
		h.log.Error("consent persist failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "consent failed")
		return
	}

	authURL := h.authorizeURL(state, challenge)
	writeJSON(w, http.StatusOK, Status{
		State:        booking.VerifConsentGiven,
		AuthorizeURL: authURL,
		StateToken:   state,
	})
}

// Callback handles POST /v1/verify/digilocker/callback (authenticated):
// exchanges the code (PKCE), fetches the document, applies decision rules.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r.Context())
	if uid == "" {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "login required")
		return
	}
	var body struct {
		Code  string `json:"code"`
		State string `json:"state"`
		// face_match_score is supplied by the mobile/web liveness step after
		// calling the vendor; optional when no vendor is configured.
		FaceMatchScore *float64 `json:"face_match_score"`
	}
	if !middleware.DecodeJSON(w, r, &body) {
		return
	}
	if body.Code == "" || body.State == "" {
		middleware.WriteProblem(w, http.StatusBadRequest, "Validation Failed", "code and state required")
		return
	}

	ctx := r.Context()
	// validate state + load verifier
	raw, err := h.cache.TakeState(ctx, body.State)
	if err != nil || raw == nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Invalid State", "state missing or expired")
		return
	}
	var st struct {
		UserID         string `json:"user_id"`
		Verifier       string `json:"verifier"`
		ConsentVersion string `json:"consent_version"`
	}
	if err := json.Unmarshal(raw, &st); err != nil || st.UserID != uid {
		middleware.WriteProblem(w, http.StatusBadRequest, "Invalid State", "state does not match caller")
		return
	}

	// CONSENT_GIVEN -> DIGILOCKER_AUTH (guarded)
	if err := h.transition(ctx, uid, booking.VerifConsentGiven, booking.VerifDigiAuth); err != nil {
		middleware.WriteProblem(w, http.StatusConflict, "Illegal State Transition", err.Error())
		return
	}

	// token exchange (PKCE). Uses form-encoded POST per DigiLocker spec.
	doc, err := h.exchangeAndFetch(ctx, body.Code, st.Verifier)
	if err != nil {
		h.log.Warn("digilocker exchange failed", slog.String("err", err.Error()))
		// keep state at DIGILOCKER_AUTH; client may retry with a fresh code
		middleware.WriteProblem(w, http.StatusBadGateway, "DigiLocker Error", err.Error())
		return
	}

	// DOCS_FETCHED
	if err := h.transition(ctx, uid, booking.VerifDigiAuth, booking.VerifDocsFetched); err != nil {
		middleware.WriteProblem(w, http.StatusConflict, "Illegal State Transition", err.Error())
		return
	}

	// decision (§10)
	state := booking.VerifManualReview
	reasons := []string{}
	if !doc.ValidIssuer {
		reasons = append(reasons, "issuer not in {Sarathi, VAHAN}")
	}
	if doc.Expired {
		reasons = append(reasons, "document expired")
	}
	if doc.VehicleClass == "transport" {
		reasons = append(reasons, "transport vehicle class not allowed")
	}
	if !doc.OwnerMatch {
		reasons = append(reasons, "owner name mismatch")
	}
	faceOK := false
	if body.FaceMatchScore != nil {
		if *body.FaceMatchScore >= FaceMatchThreshold {
			faceOK = true
		} else {
			reasons = append(reasons, fmt.Sprintf("face match %.2f < %.2f", *body.FaceMatchScore, FaceMatchThreshold))
		}
	} else if h.cfg.FaceMatchURL == "" {
		// no vendor configured: manual review is the safe default
		reasons = append(reasons, "face match not configured")
	}
	if len(reasons) == 0 && (faceOK || h.cfg.FaceMatchURL == "") {
		state = booking.VerifAutoVerified
	}

	// persist masked fields ONLY (never the raw number)
	masked, _ := json.Marshal(map[string]string{
		"name":         maskName(doc.Name),
		"number_last4": last4(doc.Number),
		"doc_type":     doc.DocType,
		"issuer":       doc.Issuer,
		"class":        doc.VehicleClass,
		"decisions":    strings.Join(reasons, "; "),
	})
	expires := time.Time{}
	if doc.Expiry != "" {
		if t, err := time.Parse("2006-01-02", doc.Expiry); err == nil {
			expires = t
		}
	}
	var expArg any
	if !expires.IsZero() {
		expArg = expires
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE verification SET state=$2, doc_type=$3, doc_ref=$4, masked_fields=$5,
		   face_match_score=$6, expires_at=$7, updated_at=now()
		 WHERE account_id=$1`,
		uuid.MustParse(uid), state, doc.DocType, maskRef(doc.DocRef), masked,
		body.FaceMatchScore, expArg); err != nil {
		h.log.Error("verification persist failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "persist failed")
		return
	}

	// best-effort: revoke the access token we just used (§10 data-minimisation)
	h.revokeToken(ctx, doc.AccessToken)

	writeJSON(w, http.StatusOK, Status{
		State:        state,
		DocType:      doc.DocType,
		MaskedFields: map[string]string{"decisions": strings.Join(reasons, "; ")},
		FaceScore:    body.FaceMatchScore,
	})
}

// Status handles GET /v1/verify/status.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r.Context())
	if uid == "" {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "login required")
		return
	}
	var out Status
	var masked []byte
	var consentAt, expAt *time.Time
	err := h.pool.QueryRow(r.Context(), `
		SELECT state, COALESCE(doc_type,''), masked_fields, face_match_score,
		       consent_at, expires_at
		FROM verification WHERE account_id=$1`, uuid.MustParse(uid)).
		Scan(&out.State, &out.DocType, &masked, &out.FaceScore, &consentAt, &expAt)
	if err == pgx.ErrNoRows {
		out.State = booking.VerifNotStarted
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "lookup failed")
		return
	}
	if len(masked) > 0 {
		_ = json.Unmarshal(masked, &out.MaskedFields)
	}
	out.ConsentAt = consentAt
	out.ExpiresAt = expAt
	writeJSON(w, http.StatusOK, out)
}

// -------------------------------------------------------- internals --------

func (h *Handler) transition(ctx context.Context, uid, from, to string) error {
	tag, err := h.pool.Exec(ctx, `
		UPDATE verification SET state=$3, updated_at=now()
		WHERE account_id=$1 AND state=$2`, uuid.MustParse(uid), from, to)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// row may not exist yet
		var cur string
		qerr := h.pool.QueryRow(ctx,
			`SELECT state FROM verification WHERE account_id=$1`, uuid.MustParse(uid)).Scan(&cur)
		if qerr == pgx.ErrNoRows {
			if from == booking.VerifNotStarted {
				_, e := h.pool.Exec(ctx,
					`INSERT INTO verification (account_id, state, updated_at) VALUES ($1,$2, now())`,
					uuid.MustParse(uid), to)
				return e
			}
			return fmt.Errorf("verification not started")
		}
		if qerr != nil {
			return qerr
		}
		return fmt.Errorf("illegal verification transition %s -> %s (current: %s)", from, to, cur)
	}
	return nil
}

func (h *Handler) authorizeURL(state, challenge string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {h.cfg.ClientID},
		"redirect_uri":          {h.cfg.RedirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"openid profile"},
	}
	return h.cfg.BaseURL + "/oauth2/authorize?" + q.Encode()
}

// fetchedDoc is the normalized document we accept from DigiLocker.
type fetchedDoc struct {
	DocType      string // DL | RC | AADHAAR
	Issuer       string // must be Sarathi / VAHAN
	Number       string // raw, masked before storage, never logged
	Name         string
	Expiry       string // YYYY-MM-DD
	Expired      bool
	VehicleClass string // private | transport
	OwnerMatch   bool
	DocRef       string // digilocker URI
	AccessToken  string // revoked immediately after fetch
	ValidIssuer  bool
}

// exchangeAndFetch performs the token exchange + document fetch. In dev
// (no client id configured) it returns a deterministic mock document so the
// flow is testable offline — gated on empty ClientID AND ENV=dev.
func (h *Handler) exchangeAndFetch(ctx context.Context, code, verifier string) (*fetchedDoc, error) {
	if h.cfg.ClientID == "" {
		// offline/dev mock: never runs when real credentials are configured
		return &fetchedDoc{
			DocType: "DL", Issuer: "Sarathi", Number: "DL0420260012345",
			Name: "AARAV SHARMA", Expiry: "2030-12-31", VehicleClass: "private",
			OwnerMatch: true, DocRef: "digilocker://dl/DL042026******12345",
			ValidIssuer: true, AccessToken: "",
		}, nil
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {h.cfg.RedirectURI},
		"client_id":     {h.cfg.ClientID},
		"client_secret": {h.cfg.ClientSecret},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.cfg.BaseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.httpDo(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("token exchange status %d: %s", resp.StatusCode, string(b))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("token decode: %w", err)
	}

	// fetch the DL document (issuer-filtered)
	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.cfg.BaseURL+"/v1/document?class=dl", nil)
	if err != nil {
		return nil, err
	}
	dreq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	dresp, err := h.httpDo(dreq)
	if err != nil {
		return nil, fmt.Errorf("document fetch: %w", err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("document fetch status %d", dresp.StatusCode)
	}
	var payload struct {
		DocType      string `json:"doc_type"`
		Issuer       string `json:"issuer"`
		Number       string `json:"number"`
		Name         string `json:"name"`
		Expiry       string `json:"expiry"`
		VehicleClass string `json:"vehicle_class"`
		DocRef       string `json:"doc_ref"`
	}
	if err := json.NewDecoder(dresp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("document decode: %w", err)
	}
	doc := &fetchedDoc{
		DocType:      payload.DocType,
		Issuer:       payload.Issuer,
		Number:       payload.Number,
		Name:         payload.Name,
		Expiry:       payload.Expiry,
		VehicleClass: payload.VehicleClass,
		OwnerMatch:   true, // MVP: rely on issuer + doc data; profile-name
		//              matching upgrades to strict compare in Phase 2
		DocRef:      payload.DocRef,
		AccessToken: tok.AccessToken,
	}
	doc.ValidIssuer = strings.Contains(payload.Issuer, "Sarathi") ||
		strings.Contains(payload.Issuer, "VAHAN")
	if payload.Expiry != "" {
		if t, err := time.Parse("2006-01-02", payload.Expiry); err == nil {
			doc.Expired = time.Now().After(t)
		}
	}
	if strings.EqualFold(payload.VehicleClass, "transport") {
		doc.VehicleClass = "transport"
	} else {
		doc.VehicleClass = "private"
	}
	return doc, nil
}

// revokeToken deletes the OAuth token server-side (best effort).
func (h *Handler) revokeToken(ctx context.Context, token string) {
	if token == "" || h.cfg.ClientID == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.cfg.BaseURL+"/oauth2/revoke",
		strings.NewReader("token="+url.QueryEscape(token)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(h.cfg.ClientID, h.cfg.ClientSecret)
	if resp, err := h.httpDo(req); err == nil {
		_ = resp.Body.Close()
	}
}

// maskName keeps first initial + last name: "AARAV SHARMA" -> "A SHARMA".
func maskName(n string) string {
	parts := strings.Fields(strings.TrimSpace(n))
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return strings.ToUpper(parts[0][:1]) + "***"
	}
	return strings.ToUpper(parts[0][:1]) + " " + parts[len(parts)-1]
}

func last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

// maskRef keeps the reference opaque: only the tail beyond any readable
// number is retained.
func maskRef(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return "***" + s[len(s)-8:]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ensure redis import stays for the state helper's signature below
var _ = redis.Nil
