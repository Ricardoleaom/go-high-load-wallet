// Package config loads the application's configuration from environment
// variables. It has no dependency on Fx, HTTP, SQS or persistence
// libraries, so it can be constructed and validated before anything else
// starts.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPPort string
	LogLevel slog.Level

	// The admin server exposes metrics and, only when PprofEnabled, the Go
	// profiler. It listens on loopback unless ADMIN_ADDR says otherwise.
	AdminAddr    string
	PprofEnabled bool

	// RequestTimeout bounds every authenticated HTTP request.
	RequestTimeout time.Duration

	DatabaseURL    string
	DBMaxOpenConns int
	DBMaxIdleConns int

	// Session limits PostgreSQL enforces on every pooled connection.
	DBStatementTimeout         time.Duration
	DBIdleInTransactionTimeout time.Duration
	DBLockTimeout              time.Duration

	JWKSURL             string
	JWKSRefreshInterval time.Duration

	AWSRegion            string
	AWSAccessKeyID       string
	AWSSecretAccessKey   string
	SQSEndpoint          string
	TransactionsQueueURL string
	EventsQueueURL       string

	OutboxPublishInterval  time.Duration
	OutboxBatchSize        int
	ReferenceRetryInterval time.Duration
}

// Load reads the environment. Defaults reproduce the behavior the service
// shipped with; a value that is set but invalid fails the boot instead of
// silently falling back to the default.
func Load() (Config, error) {
	cfg := Config{
		HTTPPort:             getEnv("HTTP_PORT", "8080"),
		AdminAddr:            getEnv("ADMIN_ADDR", "127.0.0.1:6060"),
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		JWKSURL:              os.Getenv("JWKS_URL"),
		AWSRegion:            getEnv("AWS_REGION", "us-east-1"),
		AWSAccessKeyID:       getEnv("AWS_ACCESS_KEY_ID", "test"),
		AWSSecretAccessKey:   getEnv("AWS_SECRET_ACCESS_KEY", "test"),
		SQSEndpoint:          os.Getenv("SQS_ENDPOINT"),
		TransactionsQueueURL: os.Getenv("SQS_TRANSACTIONS_QUEUE_URL"),
		EventsQueueURL:       os.Getenv("SQS_EVENTS_QUEUE_URL"),
	}

	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	var err error
	cfg.LogLevel, err = ParseLogLevel(os.Getenv("LOG_LEVEL"))
	collect(err)
	cfg.PprofEnabled, err = boolEnv("PPROF_ENABLED", false)
	collect(err)
	cfg.DBMaxOpenConns, err = intEnv("DB_MAX_OPEN_CONNS", 10, 1)
	collect(err)
	cfg.DBMaxIdleConns, err = intEnv("DB_MAX_IDLE_CONNS", 5, 0)
	collect(err)
	cfg.OutboxBatchSize, err = intEnv("OUTBOX_BATCH_SIZE", 20, 1)
	collect(err)
	cfg.OutboxPublishInterval, err = durationEnv("OUTBOX_PUBLISH_INTERVAL", 2*time.Second)
	collect(err)
	cfg.ReferenceRetryInterval, err = durationEnv("REFERENCE_RETRY_INTERVAL", 15*time.Second)
	collect(err)
	cfg.JWKSRefreshInterval, err = durationEnv("JWKS_REFRESH_INTERVAL", time.Hour)
	collect(err)
	cfg.RequestTimeout, err = durationEnv("REQUEST_TIMEOUT", 5*time.Second)
	collect(err)
	cfg.DBStatementTimeout, err = durationEnv("DB_STATEMENT_TIMEOUT", 5*time.Second)
	collect(err)
	cfg.DBIdleInTransactionTimeout, err = durationEnv("DB_IDLE_IN_TRANSACTION_TIMEOUT", 10*time.Second)
	collect(err)
	cfg.DBLockTimeout, err = durationEnv("DB_LOCK_TIMEOUT", 2*time.Second)
	collect(err)

	for key, value := range map[string]string{
		"DATABASE_URL":               cfg.DatabaseURL,
		"JWKS_URL":                   cfg.JWKSURL,
		"SQS_TRANSACTIONS_QUEUE_URL": cfg.TransactionsQueueURL,
		"SQS_EVENTS_QUEUE_URL":       cfg.EventsQueueURL,
	} {
		if value == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// ParseLogLevel accepts debug, info, warn or error in any case; empty means
// info.
func ParseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL %q is not one of debug, info, warn, error", raw)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func boolEnv(key string, fallback bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s %q is not a boolean", key, raw)
	}
	return b, nil
}

func intEnv(key string, fallback, minimum int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not an integer", key, raw)
	}
	if n < minimum {
		return 0, fmt.Errorf("%s must be at least %d, got %d", key, minimum, n)
	}
	return n, nil
}

// durationEnv accepts whole seconds ("2", the original format) or a Go
// duration ("500ms").
func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		seconds, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return 0, fmt.Errorf("%s %q is neither whole seconds nor a duration such as 500ms", key, raw)
		}
		d = time.Duration(seconds) * time.Second
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", key, raw)
	}
	return d, nil
}
