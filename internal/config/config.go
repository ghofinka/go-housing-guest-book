// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/mail"
	"os"
	"strconv"
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
	// SMTP is the optional mail server for owner notifications (PRD S3). Without SMTP_HOST the
	// feature is off and nothing is queued.
	SMTP SMTP
}

// SMTP holds the mail server settings. The password is never logged or put in an error.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	// From is the sender address, for example "Buku Tamu <noreply@example.com>".
	From string
}

// Enabled reports whether a mail server is configured.
func (s SMTP) Enabled() bool { return s.Host != "" }

// loadSMTP reads the optional SMTP_* variables. Nothing set means notifications are off. Anything set
// without SMTP_HOST, or a host without a sender, or a half-given login is a mistake worth failing on.
func loadSMTP() (SMTP, error) {
	s := SMTP{
		Host:     os.Getenv("SMTP_HOST"),
		Username: os.Getenv("SMTP_USERNAME"),
		Password: os.Getenv("SMTP_PASSWORD"),
		From:     os.Getenv("SMTP_FROM"),
	}
	port := os.Getenv("SMTP_PORT")
	if s.Host == "" {
		if s.Username != "" || s.Password != "" || s.From != "" || port != "" {
			return SMTP{}, errors.New("SMTP_HOST is required when other SMTP_* settings are set")
		}
		return SMTP{}, nil
	}
	s.Port = 587
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return SMTP{}, errors.New("SMTP_PORT must be a port number between 1 and 65535")
		}
		s.Port = n
	}
	if s.From == "" {
		return SMTP{}, errors.New("SMTP_FROM is required when SMTP_HOST is set")
	}
	if _, err := mail.ParseAddress(s.From); err != nil {
		return SMTP{}, errors.New("SMTP_FROM is not a valid email address")
	}
	if (s.Username == "") != (s.Password == "") {
		return SMTP{}, errors.New("SMTP_USERNAME and SMTP_PASSWORD must be set together")
	}
	return s, nil
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
	if cfg.SMTP, err = loadSMTP(); err != nil {
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
