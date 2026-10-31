// Package config loads API settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
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
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if (c.MobileServerKey == "") != (c.MobileAppCert == "") {
		return c, errors.New("set both LEGION_MOBILE_SERVER_KEY and LEGION_MOBILE_APP_CERT, or neither")
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
