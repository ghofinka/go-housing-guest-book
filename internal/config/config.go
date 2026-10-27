// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// MinJWTSecretBytes is the shortest accepted HS256 signing secret.
const MinJWTSecretBytes = 32

type Config struct {
	DatabaseURL     string
	HTTPAddr        string
	JWTSecret       []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	CORSOrigin      string
	// PhotoDir is where visit photos are stored (local disk, private, never served statically).
	PhotoDir string
	// RetentionEnabled turns the daily in-process retention job on or off (default on).
	// RetentionRunAt is its daily start time in WIB, "HH:MM" (default 02:00).
	RetentionEnabled bool
	RetentionRunAt   string
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		HTTPAddr:    os.Getenv("HTTP_ADDR"),
		JWTSecret:   []byte(os.Getenv("JWT_SECRET")),
		CORSOrigin:  os.Getenv("CORS_ORIGIN"),
		PhotoDir:    os.Getenv("PHOTO_DIR"),
	}
	if cfg.PhotoDir == "" {
		cfg.PhotoDir = "data/photos"
	}
	cfg.RetentionEnabled = os.Getenv("RETENTION_ENABLED") != "false"
	if cfg.RetentionRunAt = os.Getenv("RETENTION_RUN_AT"); cfg.RetentionRunAt == "" {
		cfg.RetentionRunAt = "02:00"
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	if len(cfg.JWTSecret) < MinJWTSecretBytes {
		return Config{}, fmt.Errorf("JWT_SECRET is required and must be at least %d bytes", MinJWTSecretBytes)
	}
	var err error
	if cfg.AccessTokenTTL, err = durationEnv("ACCESS_TOKEN_TTL", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.RefreshTokenTTL, err = durationEnv("REFRESH_TOKEN_TTL", 7*24*time.Hour); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 15m", key)
	}
	return d, nil
}
