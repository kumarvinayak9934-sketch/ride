// Package auth implements §5.1 phone-OTP authentication:
//
//	POST /v1/auth/otp/request — 3 requests / 10 min / number (Redis window)
//	POST /v1/auth/otp/verify  — checks code, issues a 24 h HS256 JWT
//
// Phone numbers are never stored raw: keyed by HMAC-SHA256(pepper, E.164),
// encrypted at rest with AES-256-GCM (DPDP §10).
package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/cache"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/config"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/middleware"
)

var phoneRe = regexp.MustCompile(`^\+?[1-9]\d{6,14}$`)

// Store is the persistence surface auth needs (swapped in tests).
type Store interface {
	FindOrCreateAccount(ctx context.Context, phoneHash, phoneEnc []byte, ageConfirmed bool) (id uuid.UUID, role string, err error)
}

type pgStore struct{ pool *pgxpool.Pool }

func (s *pgStore) FindOrCreateAccount(ctx context.Context, phoneHash, phoneEnc []byte, ageConfirmed bool) (uuid.UUID, string, error) {
	if !ageConfirmed {
		return uuid.Nil, "", errors.New("age confirmation (18+) required")
	}
	var id uuid.UUID
	var role string
	// Users arrive via OTP; we create on first successful verification.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO account (phone_hash, phone_enc, age_confirmed, role)
		VALUES ($1, $2, true, 'rider')
		ON CONFLICT (phone_hash) DO UPDATE SET updated_at = now(), age_confirmed = true
		RETURNING id, role`, phoneHash, phoneEnc).Scan(&id, &role)
	return id, role, err
}

// Service wires OTP + JWT.
type Service struct {
	cfg   *config.Config
	cache *cache.Cache
	store Store
	log   *slog.Logger
	// sendSMS delivers an OTP; swapped for a stub in dev/tests.
	sendSMS func(ctx context.Context, phone, code string) error
}

func NewService(cfg *config.Config, c *cache.Cache, store Store, log *slog.Logger) *Service {
	s := &Service{cfg: cfg, cache: c, store: store, log: log}
	s.sendSMS = func(ctx context.Context, phone, code string) error {
		if cfg.OTPVDevEcho {
			log.Info("dev OTP (OTP_DEV_ECHO=true)", slog.String("phone", mask(phone)), slog.String("code", code))
			return nil
		}
		// Production: plug an SMS gateway here (MSG91/Exotel/AWS SNS).
		log.Info("SMS dispatch stub — configure gateway", slog.String("phone", mask(phone)))
		return nil
	}
	return s
}

// NewPGStore builds the default store.
func NewPGStore(pool *pgxpool.Pool) Store { return &pgStore{pool: pool} }

func mask(p string) string {
	if len(p) < 6 {
		return "***"
	}
	return p[:3] + strings.Repeat("*", len(p)-6) + p[len(p)-3:]
}

// HashPhone = HMAC-SHA256(pepper, phone) as hex (lookup key).
func (s *Service) HashPhone(phone string) string {
	m := hmac.New(sha256.New, []byte(s.cfg.PhoneHashPepper))
	m.Write([]byte(phone))
	return hex.EncodeToString(m.Sum(nil))
}

// EncryptPhone AES-256-GCM encrypts the raw number for phone_enc.
func (s *Service) EncryptPhone(phone string) ([]byte, error) {
	block, err := aes.NewCipher(s.cfg.PhoneEncKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(phone), nil), nil
}

// NormalizePhone validates + canonicalises to E.164-ish (+91XXXXXXXXXX for
// 10-digit Indian numbers; other country codes pass through as typed).
func NormalizePhone(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	p = strings.ReplaceAll(p, " ", "")
	p = strings.ReplaceAll(p, "-", "")
	if p == "" {
		return "", errors.New("phone is required")
	}
	if !strings.HasPrefix(p, "+") {
		if len(p) == 10 && p[0] >= '6' && p[0] <= '9' {
			p = "+91" + p
		} else if strings.HasPrefix(p, "0") && len(p) == 11 {
			p = "+91" + p[1:]
		} else {
			p = "+" + p
		}
	}
	if !phoneRe.MatchString(p) {
		return "", errors.New("invalid phone number")
	}
	return p, nil
}

// ---------------------------------------------------------- handlers ------

// RequestOTP handles POST /v1/auth/otp/request.
// Rate limit: 3 per 10 minutes per number (§5.1), enforced in Redis.
func (s *Service) RequestOTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if !middleware.DecodeJSON(w, r, &body) {
		return
	}
	phone, err := NormalizePhone(body.Phone)
	if err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Invalid Phone", err.Error())
		return
	}
	hash := s.HashPhone(phone)

	// 3 / 10 min / number
	count, allowed, err := s.cache.FixedWindow(r.Context(), "otp:"+hash, 10*time.Minute, s.cfg.OTPReqPer10Min)
	if err != nil {
		s.log.Warn("otp rate limit unavailable", slog.String("err", err.Error()))
	}
	if !allowed {
		middleware.WriteProblem(w, http.StatusTooManyRequests, "Too Many Requests",
			fmt.Sprintf("limited to %d OTP requests per 10 minutes", s.cfg.OTPReqPer10Min))
		return
	}
	_ = count

	code := randNumeric(6)
	if err := s.cache.PutOTP(r.Context(), hash, code); err != nil {
		middleware.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "try again shortly")
		return
	}
	if err := s.sendSMS(r.Context(), phone, code); err != nil {
		s.log.Error("sms dispatch failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusServiceUnavailable, "SMS Failed", "could not send OTP")
		return
	}
	resp := map[string]any{
		"status":     "sent",
		"expires_in": 600,
		"masked":     mask(phone),
	}
	if s.cfg.OTPVDevEcho {
		resp["dev_otp"] = code // dev only; NEVER enable in prod
	}
	writeJSON(w, http.StatusOK, resp)
}

// VerifyOTP handles POST /v1/auth/otp/verify — checks the code, upserts the
// account, and issues the session JWT.
func (s *Service) VerifyOTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone        string `json:"phone"`
		Code         string `json:"code"`
		AgeConfirmed bool   `json:"age_confirmed"` // 18+ age gate (DPDP)
	}
	if !middleware.DecodeJSON(w, r, &body) {
		return
	}
	phone, err := NormalizePhone(body.Phone)
	if err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Invalid Phone", err.Error())
		return
	}
	hash := s.HashPhone(phone)

	stored, ok, err := s.cache.TakeOTP(r.Context(), hash)
	if err != nil {
		middleware.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "try again shortly")
		return
	}
	if !ok {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Invalid or Expired OTP", "request a new code")
		return
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(strings.TrimSpace(body.Code))) != 1 {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Invalid OTP", "code does not match")
		return
	}
	if !body.AgeConfirmed {
		middleware.WriteProblem(w, http.StatusForbidden, "Age Confirmation Required",
			"you must confirm you are 18 or older to use Safar Share")
		return
	}

	phoneEnc, err := s.EncryptPhone(phone)
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "crypto failure")
		return
	}
	id, role, err := s.store.FindOrCreateAccount(r.Context(), []byte(hash), phoneEnc, body.AgeConfirmed)
	if err != nil {
		if strings.Contains(err.Error(), "age confirmation") {
			middleware.WriteProblem(w, http.StatusForbidden, "Age Confirmation Required", err.Error())
			return
		}
		s.log.Error("account upsert failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "account lookup failed")
		return
	}

	token, expiresAt, err := s.IssueToken(id, role)
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "token issue failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"token_type": "Bearer",
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"user":       map[string]any{"id": id, "role": role},
	})
}

// Logout handles POST /v1/auth/logout — revokes the presented JTI.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		if claims, err := s.parseClaims(strings.TrimPrefix(h, "Bearer ")); err == nil && claims.ID != "" {
			ttl := time.Until(time.Unix(claims.ExpiresAt.Unix(), 0))
			if ttl > 0 {
				_ = s.cache.BlockJTI(r.Context(), claims.ID, ttl)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// ------------------------------------------------------------- JWT --------

// IssueToken mints a 24 h HS256 token (sub=user id, jti for revocation).
func (s *Service) IssueToken(userID uuid.UUID, role string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(24 * time.Hour)
	claims := &middleware.Claims{
		UserID: userID.String(),
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			Issuer:    "safar-share",
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := t.SignedString([]byte(s.cfg.JWTSecret))
	return signed, exp, err
}

func (s *Service) parseClaims(tokenStr string) (*middleware.Claims, error) {
	claims := &middleware.Claims{}
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		return []byte(s.cfg.JWTSecret), nil
	})
	return claims, err
}

// ------------------------------------------------------------ helpers ------

func randNumeric(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = '0' + b[i]%10
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ensure pgx is referenced (used by pgStore query)
var _ = pgx.ErrNoRows
