// Package config loads the application's configuration from environment
// variables. It has no dependency on Fx, HTTP, SQS or persistence
// libraries, so it can be constructed and validated before anything else
// starts.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPPort string

	DatabaseURL string

	JWKSURL             string
	JWKSRefreshInterval time.Duration

	AWSRegion            string
	AWSAccessKeyID       string
	AWSSecretAccessKey   string
	SQSEndpoint          string
	TransactionsQueueURL string
	EventsQueueURL       string

	OutboxPublishInterval  time.Duration
	ReferenceRetryInterval time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		HTTPPort:               getEnv("HTTP_PORT", "8080"),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		JWKSURL:                os.Getenv("JWKS_URL"),
		JWKSRefreshInterval:    getDuration("JWKS_REFRESH_INTERVAL", time.Hour),
		AWSRegion:              getEnv("AWS_REGION", "us-east-1"),
		AWSAccessKeyID:         getEnv("AWS_ACCESS_KEY_ID", "test"),
		AWSSecretAccessKey:     getEnv("AWS_SECRET_ACCESS_KEY", "test"),
		SQSEndpoint:            os.Getenv("SQS_ENDPOINT"),
		TransactionsQueueURL:   os.Getenv("SQS_TRANSACTIONS_QUEUE_URL"),
		EventsQueueURL:         os.Getenv("SQS_EVENTS_QUEUE_URL"),
		OutboxPublishInterval:  getDuration("OUTBOX_PUBLISH_INTERVAL", 2*time.Second),
		ReferenceRetryInterval: getDuration("REFERENCE_RETRY_INTERVAL", 15*time.Second),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.JWKSURL == "" {
		return Config{}, fmt.Errorf("JWKS_URL is required")
	}
	if cfg.TransactionsQueueURL == "" {
		return Config{}, fmt.Errorf("SQS_TRANSACTIONS_QUEUE_URL is required")
	}
	if cfg.EventsQueueURL == "" {
		return Config{}, fmt.Errorf("SQS_EVENTS_QUEUE_URL is required")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
