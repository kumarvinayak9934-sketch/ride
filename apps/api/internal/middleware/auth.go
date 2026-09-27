package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/cache"
)

// Claims is the session JWT (HS256, 24 h per §5.1).
type Claims struct {
	UserID string `json:"uid"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

type userKey struct{}

// UserID extracts the authenticated user id from the request context.
func UserID(ctx context.Context) string {
	if v, ok := ctx.Value(userKey{}).(string); ok {
		return v
	}
	return ""
}

// UserAuth validates Authorization: Bearer <JWT> and injects the user id.
func UserAuth(secret string, c *cache.Cache) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "missing bearer token")
				return
			}
			tokenStr := strings.TrimPrefix(h, "Bearer ")
			claims := &Claims{}
			token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok || t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
					return nil, jwt.ErrSignatureInvalid
				}
				return []byte(secret), nil
			})
			if err != nil || !token.Valid {
				WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "invalid or expired token")
				return
			}
			if claims.ExpiresAt != nil && time.Now().After(claims.ExpiresAt.Time) {
				WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "token expired")
				return
			}
			// revoked (logout)
			if claims.ID != "" {
				if blocked, err := c.IsBlocked(r.Context(), claims.ID); err == nil && blocked {
					WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "token revoked")
					return
				}
			}
			ctx := context.WithValue(r.Context(), userKey{}, claims.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ---------------------------------------------------------------- CORS ----

// CORS allows configured origins with credentials; preflights short-circuit.
func CORS(origins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[strings.TrimSpace(o)] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// DecodeJSON strict-decodes a JSON body with a size cap (1 MiB).
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		WriteProblem(w, http.StatusBadRequest, "Invalid Request Body", err.Error())
		return false
	}
	return true
}
