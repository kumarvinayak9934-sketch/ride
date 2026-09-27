// Package config loads runtime configuration from the environment.
// Every value has a safe dev default; production must override the secrets.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env      string
	HTTPAddr string

	DatabaseURL string
	RedisAddr   string
	RedisPass   string

	JWTSecret       string
	PhoneHashPepper string
	PhoneEncKey     []byte // 32 bytes for AES-256-GCM
	WebhookSecret   []byte // HMAC-SHA256 key for payment webhooks

	OTPVDevEcho     bool // OTP_DEV_ECHO: return OTP in response (dev only)
	RateLimitPerMin int
	OTPReqPer10Min  int

	Pricing PricingConfig
	Match   MatchConfig

	UPILocation string // merchant VPA

	DigiLocker DigiLockerConfig

	AllowedOrigins []string
}

// PricingConfig holds the §5.3 fare inputs. All money values are rupees.
type PricingConfig struct {
	BaseFare             float64 // ₹ flat pickup charge
	KMFare               float64 // ₹ per km
	MinFarePerMin        float64 // ₹ per minute of ride time
	MinFare              float64 // floor per seat
	SurgeCap             float64 // hard multiplier ceiling (spec: 1.5)
	IntercityThresholdKm float64 // distance at/above which intercity fees apply
	ConvFeeLocal         float64 // ₹ convenience fee, local trips
	ConvFeeIntercity     float64 // ₹ convenience fee, intercity trips
	GSTPct               float64 // GST on convenience fee (18)

	// Cost-cap inputs (mandatory clamp): what a seat costs the driver.
	CostFuel    float64 // ₹ per litre
	CostMileage float64 // km per litre
	CostMaint   float64 // ₹ per km
	CostTolls   float64 // ₹ flat tolls per trip (0 unless route-specific)
}

// MatchConfig carries §5.2 corridor search + ranking knobs.
type MatchConfig struct {
	RadiusM     float64            // corridor search radius (spec: 2000 m)
	RankWeights map[string]Weights // per-city override; "" key = default
}

// Weights is the ranking mix; must sum to ~1.0. Defaults: 0.40/0.25/0.20/0.15.
type Weights struct {
	Detour float64 `json:"detour"`
	Time   float64 `json:"time"`
	Rating float64 `json:"rating"`
	Price  float64 `json:"price"`
}

// DefaultWeights is the spec's §5.2 ranking mix.
func DefaultWeights() Weights {
	return Weights{Detour: 0.40, Time: 0.25, Rating: 0.20, Price: 0.15}
}

type DigiLockerConfig struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	FaceMatchURL string // vendor endpoint; empty disables face match (-> MANUAL_REVIEW)
}

func get(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getf(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return def
}

func geti(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		i, err := strconv.Atoi(v)
		if err == nil {
			return i
		}
	}
	return def
}

func getb(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

func getlist(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getbytes(key, defHex string) []byte {
	v := get(key, defHex)
	out := make([]byte, len(v)/2)
	for i := 0; i < len(out); i++ {
		var b int
		fmt.Sscanf(v[i*2:i*2+2], "%02x", &b)
		out[i] = byte(b)
	}
	return out
}

// Load reads configuration from the environment with dev-safe defaults.
func Load() (*Config, error) {
	c := &Config{
		Env:             get("ENV", "dev"),
		HTTPAddr:        get("HTTP_ADDR", ":8080"),
		DatabaseURL:     get("DATABASE_URL", ""),
		RedisAddr:       get("REDIS_ADDR", "localhost:6379"),
		RedisPass:       get("REDIS_PASSWORD", ""),
		JWTSecret:       get("JWT_SECRET", "dev-only-change-me-0123456789abcdef"),
		PhoneHashPepper: get("PHONE_HASH_PEPPER", "dev-pepper-change-me"),
		PhoneEncKey:     getbytes("PHONE_ENC_KEY", "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"),
		WebhookSecret:   getbytes("WEBHOOK_SECRET", "dev-webhook-secret-change-me"),

		OTPVDevEcho:     getb("OTP_DEV_ECHO", true),
		RateLimitPerMin: geti("RATE_LIMIT_PER_MIN", 600),
		OTPReqPer10Min:  geti("OTP_RATE_LIMIT_PER_10MIN", 3),

		UPILocation: get("UPI_VPA", "safarshare@upi"),

		Pricing: PricingConfig{
			BaseFare:             getf("PRICING_BASE_FARE", 15),
			KMFare:               getf("PRICING_KM_FARE", 4.5),
			MinFarePerMin:        getf("PRICING_MIN_FARE_PER_MIN", 0.5),
			MinFare:              getf("PRICING_MIN_FARE", 40),
			SurgeCap:             getf("PRICING_SURGE_CAP", 1.5),
			IntercityThresholdKm: getf("PRICING_INTERCITY_THRESHOLD_KM", 50),
			ConvFeeLocal:         getf("PRICING_CONV_FEE_LOCAL", 19),
			ConvFeeIntercity:     getf("PRICING_CONV_FEE_INTERCITY", 29),
			GSTPct:               getf("PRICING_GST_PCT", 18),
			CostFuel:             getf("COSTCAP_FUEL", 104),
			CostMileage:          getf("COSTCAP_MILEAGE", 15),
			CostMaint:            getf("COSTCAP_MAINT", 1.8),
			CostTolls:            getf("COSTCAP_TOLLS", 0),
		},

		Match: MatchConfig{
			RadiusM:     getf("MATCH_RADIUS_M", 2000),
			RankWeights: map[string]Weights{},
		},

		DigiLocker: DigiLockerConfig{
			BaseURL:      get("DIGILOCKER_BASE_URL", "https://api.digilocker.gov.in"),
			ClientID:     get("DIGILOCKER_CLIENT_ID", ""),
			ClientSecret: get("DIGILOCKER_CLIENT_SECRET", ""),
			RedirectURI:  get("DIGILOCKER_REDIRECT_URI", ""),
			FaceMatchURL: get("FACE_MATCH_URL", ""),
		},

		AllowedOrigins: getlist("ALLOWED_ORIGINS", []string{"http://localhost:3000"}),
	}

	if err := c.loadWeights(); err != nil {
		return nil, err
	}
	if len(c.PhoneEncKey) != 32 {
		return nil, fmt.Errorf("PHONE_ENC_KEY must decode to 32 bytes, got %d", len(c.PhoneEncKey))
	}
	if c.Env == "prod" && c.JWTSecret == "dev-only-change-me-0123456789abcdef" {
		return nil, fmt.Errorf("JWT_SECRET must be overridden in prod")
	}
	return c, nil
}

// loadWeights parses RANK_WEIGHTS (JSON map city -> weights). Unknown cities
// fall back to DefaultWeights via WeightsFor.
func (c *Config) loadWeights() error {
	raw := os.Getenv("RANK_WEIGHTS")
	if raw == "" {
		return nil
	}
	var m map[string]Weights
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return fmt.Errorf("RANK_WEIGHTS: %w", err)
	}
	c.Match.RankWeights = m
	return nil
}

// WeightsFor returns ranking weights for a city, defaulting to the §5.2 mix.
func (m MatchConfig) WeightsFor(city string) Weights {
	if w, ok := m.RankWeights[city]; ok {
		// guard against a bad config: never ship weights that sum to 0
		s := w.Detour + w.Time + w.Rating + w.Price
		if s > 0.5 && s < 1.5 {
			return w
		}
	}
	return DefaultWeights()
}
