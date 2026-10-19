// Package pgtest gives integration tests a migrated PostgreSQL database:
// the one in DATABASE_URL when it is set, otherwise a throwaway container
// shared by every test in the same test binary.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/hostport"
	"github.com/Ricardoleaom/go-high-load-wallet/migrations"
)

const image = "postgres:17-alpine"

var (
	once      sync.Once
	sharedURL string
	sharedErr error
)

// URL returns the connection string of a migrated database. It skips the
// test under -short and fails it when no database can be provided, so an
// integration test never passes by silently not running.
func URL(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	once.Do(func() { sharedURL, sharedErr = provision() })
	if sharedErr != nil {
		t.Fatal(sharedErr)
	}
	return sharedURL
}

// FreshURL returns a migrated database of its own on the same server as URL,
// dropped when the test ends. Tests that truncate tables or count rows
// globally use it so they cannot disturb, or be disturbed by, other tests.
func FreshURL(t testing.TB) string {
	t.Helper()
	server := URL(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("name a fresh database: %v", err)
	}
	name := "wallet_" + hex.EncodeToString(suffix)

	admin, err := sql.Open("pgx", server)
	if err != nil {
		t.Fatalf("open %s: %v", server, err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() { dropDatabase(server, name) })

	parsed, err := url.Parse(server)
	if err != nil {
		t.Fatalf("parse %s: %v", server, err)
	}
	parsed.Path = "/" + name
	fresh := parsed.String()

	if err := Migrate(ctx, fresh); err != nil {
		t.Fatalf("migrate %s: %v", name, err)
	}
	return fresh
}

func dropDatabase(server, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", server)
	if err != nil {
		return
	}
	defer func() { _ = admin.Close() }()
	_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
}

func provision() (string, error) {
	// Generous: the first run on a machine pulls the image.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		var err error
		if url, err = startContainer(ctx); err != nil {
			return "", unavailableError(err)
		}
	}

	if err := Migrate(ctx, url); err != nil {
		return "", err
	}
	return url, nil
}

// startContainer leaves cleanup to Ryuk, which removes the container when
// the test binary exits: every test in the binary shares it. The host port
// is pinned (see hostport) rather than left to Docker.
func startContainer(ctx context.Context) (string, error) {
	ctr, err := postgres.Run(ctx, image,
		postgres.WithDatabase("wallet"),
		postgres.WithUsername("app"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
		hostport.Publish("5432/tcp"),
	)
	if err != nil {
		return "", err
	}
	return ctr.ConnectionString(ctx, "sslmode=disable")
}

func unavailableError(cause error) error {
	return fmt.Errorf("no PostgreSQL for integration tests: set DATABASE_URL or make Docker available, or run with -short to skip them: %w", cause)
}

// Migrate applies every pending migration. It is a no-op on an up-to-date
// database and safe to call from several processes at once: golang-migrate
// serializes runs with a PostgreSQL advisory lock.
func Migrate(ctx context.Context, url string) error {
	return withMigrate(ctx, url, func(m *migrate.Migrate) error {
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("apply migrations: %w", err)
		}
		return nil
	})
}

// MigrateTo moves the schema to exactly version, up or down, so a test of
// one migration keeps working as later ones are added.
func MigrateTo(ctx context.Context, url string, version uint) error {
	return withMigrate(ctx, url, func(m *migrate.Migrate) error {
		if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("migrate to version %d: %w", version, err)
		}
		return nil
	})
}

func withMigrate(ctx context.Context, url string, run func(*migrate.Migrate) error) error {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("load embedded migrations: %w", err)
	}

	db, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("open database for migrations: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("ping database for migrations: %w", err)
	}

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("prepare migration driver: %w", err)
	}

	// From here on, closing m closes the driver, which closes db.
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		_ = driver.Close()
		return fmt.Errorf("prepare migrations: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	return run(m)
}
