// Package config loads API settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Addr        string
	DatabaseURL string

	// JWTSecret signs access tokens (HS256). Must be at least 32 bytes.
	JWTSecret  []byte
	AccessTTL  time.Duration
	RefreshTTL time.Duration

	// CookieSecure marks the refresh cookie Secure. Off only for local HTTP dev.
	CookieSecure bool

	// DefaultCallingCode is used to normalise local phone numbers (0712...) to E.164.
	DefaultCallingCode string

	// MigrateOnStart applies pending goose migrations before serving.
	MigrateOnStart bool

	// MobileServerKey and MobileAppCert are the PEM files of the legacy
	// mobile app's encryption (10.184.38.64.key and mobile.crt). The mobile
	// app API is off unless both are set.
	MobileServerKey string
	MobileAppCert   string

	// Location is the calendar groups bill in (LEGION_TIMEZONE).
	Location *time.Location
	// BillingAt is the daily time of the invoicing and fines run in
	// Location (LEGION_BILLING_AT, "off" turns the scheduler off).
	BillingAt string

	// WebURL is the web app's address (LEGION_WEB_URL), for the links in
	// invite texts, e.g. https://legion.com.
	WebURL string
	// Africa's Talking SMS (LEGION_AT_USERNAME, LEGION_AT_API_KEY and the
	// optional LEGION_AT_SENDER_ID). Without them texts are only logged.
	ATUsername string
	ATAPIKey   string
	ATSenderID string

	// Payments (milestone 6). PublicURL is where Chamasoft can call back,
	// normally the site itself (LEGION_PUBLIC_URL, else LEGION_WEB_URL).
	PublicURL      string
	HookKey        []byte
	ChamasoftURL   string
	ChamasoftAuth  string
	ChamasoftCert  string
	ChamasoftFake  bool
	ChamasoftIPs   []string
	EquityUsername string
	EquityPassword string
	EquityIPs      []string
	CoopUsername   string
	CoopPassword   string
	CoopIPs        []string

	// USSD and WhatsApp (milestone 7).
	USSDIPs     []string
	TwilioToken string
	Brand       string
}

func list(key string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// LoadEnvFile reads KEY=VALUE pairs from LEGION_ENV_FILE, or ./.env when
// that is unset, into the process environment. Variables already set in the
// environment win. A missing default .env is not an error.
func LoadEnvFile() error {
	path, explicit := os.LookupEnv("LEGION_ENV_FILE")
	if !explicit {
		path = ".env"
	}
	err := godotenv.Load(path)
	if err != nil && !explicit && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load env file %s: %w", path, err)
	}
	return nil
}

// Load reads the server configuration from the environment. Call
// LoadEnvFile first to pick up a .env file.
func Load() (Config, error) {
	c := Config{
		Addr:               getenv("LEGION_ADDR", ":8080"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		JWTSecret:          []byte(os.Getenv("LEGION_JWT_SECRET")),
		DefaultCallingCode: getenv("LEGION_DEFAULT_CALLING_CODE", "254"),
		MobileServerKey:    os.Getenv("LEGION_MOBILE_SERVER_KEY"),
		MobileAppCert:      os.Getenv("LEGION_MOBILE_APP_CERT"),
		BillingAt:          getenv("LEGION_BILLING_AT", "00:30"),
		WebURL:             os.Getenv("LEGION_WEB_URL"),
		ATUsername:         os.Getenv("LEGION_AT_USERNAME"),
		ATAPIKey:           os.Getenv("LEGION_AT_API_KEY"),
		ATSenderID:         os.Getenv("LEGION_AT_SENDER_ID"),
		PublicURL:          getenv("LEGION_PUBLIC_URL", os.Getenv("LEGION_WEB_URL")),
		HookKey:            []byte(os.Getenv("LEGION_HOOK_KEY")),
		ChamasoftURL:       os.Getenv("LEGION_CHAMASOFT_URL"),
		ChamasoftAuth:      os.Getenv("LEGION_CHAMASOFT_AUTH"),
		ChamasoftCert:      os.Getenv("LEGION_CHAMASOFT_CERT"),
		ChamasoftIPs:       list("LEGION_CHAMASOFT_IPS"),
		EquityUsername:     os.Getenv("LEGION_EQUITY_USERNAME"),
		EquityPassword:     os.Getenv("LEGION_EQUITY_PASSWORD"),
		EquityIPs:          list("LEGION_EQUITY_IPS"),
		CoopUsername:       os.Getenv("LEGION_COOP_USERNAME"),
		CoopPassword:       os.Getenv("LEGION_COOP_PASSWORD"),
		CoopIPs:            list("LEGION_COOP_IPS"),
		USSDIPs:            list("LEGION_USSD_IPS"),
		TwilioToken:        os.Getenv("LEGION_TWILIO_AUTH_TOKEN"),
		Brand:              getenv("LEGION_BRAND", "Legion"),
	}
	var err error
	if c.AccessTTL, err = time.ParseDuration(getenv("LEGION_ACCESS_TTL", "15m")); err != nil {
		return c, fmt.Errorf("LEGION_ACCESS_TTL: %w", err)
	}
	if c.RefreshTTL, err = time.ParseDuration(getenv("LEGION_REFRESH_TTL", "720h")); err != nil {
		return c, fmt.Errorf("LEGION_REFRESH_TTL: %w", err)
	}
	if c.CookieSecure, err = strconv.ParseBool(getenv("LEGION_COOKIE_SECURE", "true")); err != nil {
		return c, fmt.Errorf("LEGION_COOKIE_SECURE: %w", err)
	}
	if c.MigrateOnStart, err = strconv.ParseBool(getenv("LEGION_MIGRATE_ON_START", "false")); err != nil {
		return c, fmt.Errorf("LEGION_MIGRATE_ON_START: %w", err)
	}
	if c.Location, err = time.LoadLocation(getenv("LEGION_TIMEZONE", "Africa/Nairobi")); err != nil {
		return c, fmt.Errorf("LEGION_TIMEZONE: %w", err)
	}
	if c.ChamasoftFake, err = strconv.ParseBool(getenv("LEGION_CHAMASOFT_FAKE", "false")); err != nil {
		return c, fmt.Errorf("LEGION_CHAMASOFT_FAKE: %w", err)
	}
	if len(c.HookKey) == 0 {
		c.HookKey = c.JWTSecret
	}
	if c.ChamasoftURL != "" && c.ChamasoftAuth == "" {
		return c, errors.New("LEGION_CHAMASOFT_URL needs LEGION_CHAMASOFT_AUTH (user:secret)")
	}
	if (c.EquityUsername == "") != (c.EquityPassword == "") || (c.CoopUsername == "") != (c.CoopPassword == "") {
		return c, errors.New("set a bank feed's username and password together")
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if (c.MobileServerKey == "") != (c.MobileAppCert == "") {
		return c, errors.New("set both LEGION_MOBILE_SERVER_KEY and LEGION_MOBILE_APP_CERT, or neither")
	}
	if (c.ATUsername == "") != (c.ATAPIKey == "") {
		return c, errors.New("set both LEGION_AT_USERNAME and LEGION_AT_API_KEY, or neither")
	}
	if len(c.JWTSecret) < 32 {
		return c, errors.New("LEGION_JWT_SECRET must be at least 32 bytes")
	}
	return c, nil
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
