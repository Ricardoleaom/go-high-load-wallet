package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://app:test@localhost:5432/wallet")
	t.Setenv("JWKS_URL", "http://localhost:8081/certs")
	t.Setenv("SQS_TRANSACTIONS_QUEUE_URL", "http://localhost:4566/000000000000/in.fifo")
	t.Setenv("SQS_EVENTS_QUEUE_URL", "http://localhost:4566/000000000000/out.fifo")
	t.Setenv("JWT_ISSUER", "http://localhost:8081/realms/wagering")
	t.Setenv("JWT_AUDIENCE", "wallet-api")
}

func TestLoadDefaultsReproduceTheDeliveredBehavior(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"DBMaxOpenConns", cfg.DBMaxOpenConns, 10},
		{"DBMinConns", cfg.DBMinConns, 2},
		{"OutboxBatchSize", cfg.OutboxBatchSize, 20},
		{"OutboxPublishInterval", cfg.OutboxPublishInterval, 2 * time.Second},
		{"ReferenceRetryInterval", cfg.ReferenceRetryInterval, 15 * time.Second},
		{"JWKSRefreshInterval", cfg.JWKSRefreshInterval, time.Hour},
		{"AdminAddr", cfg.AdminAddr, "127.0.0.1:6060"},
		{"PprofEnabled", cfg.PprofEnabled, false},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadAcceptsSecondsAndGoDurations(t *testing.T) {
	for raw, want := range map[string]time.Duration{"1": time.Second, "500ms": 500 * time.Millisecond} {
		t.Run(raw, func(t *testing.T) {
			setRequired(t)
			t.Setenv("OUTBOX_PUBLISH_INTERVAL", raw)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.OutboxPublishInterval != want {
				t.Fatalf("OutboxPublishInterval = %v, want %v", cfg.OutboxPublishInterval, want)
			}
		})
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct{ key, value string }{
		{"DB_MAX_OPEN_CONNS", "0"},
		{"DB_MAX_OPEN_CONNS", "ten"},
		{"DB_MIN_CONNS", "-1"},
		{"OUTBOX_BATCH_SIZE", "0"},
		{"OUTBOX_PUBLISH_INTERVAL", "abc"},
		{"OUTBOX_PUBLISH_INTERVAL", "0"},
		{"REFERENCE_RETRY_INTERVAL", "-5"},
		{"LOG_LEVEL", "loud"},
		{"PPROF_ENABLED", "maybe"},
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			setRequired(t)
			t.Setenv(c.key, c.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %s=%q", c.key, c.value)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Fatalf("error %q does not name %s", err, c.key)
			}
		})
	}
}

func TestParseLogLevel(t *testing.T) {
	for raw, want := range map[string]slog.Level{"DEBUG": slog.LevelDebug, "": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		got, err := ParseLogLevel(raw)
		if err != nil || got != want {
			t.Errorf("ParseLogLevel(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := ParseLogLevel("x"); err == nil {
		t.Error("ParseLogLevel accepted an unknown level")
	}
}

func TestLoadReadsTimeouts(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range []struct {
		name      string
		got, want time.Duration
	}{
		{"RequestTimeout", cfg.RequestTimeout, 5 * time.Second},
		{"DBStatementTimeout", cfg.DBStatementTimeout, 5 * time.Second},
		{"DBIdleInTransactionTimeout", cfg.DBIdleInTransactionTimeout, 10 * time.Second},
		{"DBLockTimeout", cfg.DBLockTimeout, 2 * time.Second},
	} {
		if c.got != c.want {
			t.Errorf("default %s = %v, want %v", c.name, c.got, c.want)
		}
	}

	t.Setenv("REQUEST_TIMEOUT", "750ms")
	t.Setenv("DB_STATEMENT_TIMEOUT", "3")
	t.Setenv("DB_IDLE_IN_TRANSACTION_TIMEOUT", "1m")
	t.Setenv("DB_LOCK_TIMEOUT", "500ms")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RequestTimeout != 750*time.Millisecond || cfg.DBStatementTimeout != 3*time.Second ||
		cfg.DBIdleInTransactionTimeout != time.Minute || cfg.DBLockTimeout != 500*time.Millisecond {
		t.Fatalf("timeouts = %v %v %v %v", cfg.RequestTimeout, cfg.DBStatementTimeout,
			cfg.DBIdleInTransactionTimeout, cfg.DBLockTimeout)
	}

	for _, key := range []string{"REQUEST_TIMEOUT", "DB_STATEMENT_TIMEOUT", "DB_IDLE_IN_TRANSACTION_TIMEOUT", "DB_LOCK_TIMEOUT"} {
		t.Run(key, func(t *testing.T) {
			setRequired(t)
			t.Setenv(key, "soon")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Load error = %v, want one naming %s", err, key)
			}
		})
	}
}

func TestLoadRequiresJWTIssuerAndAudience(t *testing.T) {
	for _, key := range []string{"JWT_ISSUER", "JWT_AUDIENCE"} {
		t.Run(key, func(t *testing.T) {
			setRequired(t)
			t.Setenv(key, "")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Load error = %v, want one naming %s", err, key)
			}
		})
	}
}
