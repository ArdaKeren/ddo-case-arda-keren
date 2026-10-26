// Package testutil provides a migrated PostgreSQL database for integration tests.
//
// By default each test package starts its own throwaway postgres:16-alpine
// container with Testcontainers, so packages can run in parallel and nothing
// touches a real database. Set TEST_DATABASE_URL to use an existing disposable
// database instead (then run with `go test -p 1`, since packages share it).
//
// Without Docker and without TEST_DATABASE_URL the tests are skipped, unless
// TESTCONTAINERS_REQUIRED=1 (use that in CI) turns the skip into a failure.
package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/qa/backend/migrations"
)

const image = "postgres:16-alpine"

var (
	startOnce  sync.Once
	dsn        string
	startErr   error
	skipReason string
	container  *postgres.PostgresContainer

	migrateOnce sync.Once
	migrateErr  error
)

// Main runs the package's tests and then removes the container. Call it from
// TestMain in every package that uses this helper.
func Main(m *testing.M) {
	code := m.Run()
	if container != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := container.Terminate(ctx); err != nil {
			log.Printf("testutil: terminate container: %v", err)
		}
		cancel()
	}
	os.Exit(code)
}

// DSN returns a connection string for the test server, starting it on first use.
func DSN(t *testing.T) string {
	t.Helper()
	startOnce.Do(start)
	if skipReason != "" {
		if os.Getenv("TESTCONTAINERS_REQUIRED") == "1" {
			t.Fatalf("integration tests required but unavailable: %s", skipReason)
		}
		t.Skip(skipReason)
	}
	if startErr != nil {
		t.Fatalf("start PostgreSQL: %v", startErr)
	}
	return dsn
}

func start() {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		dsn = v
		return
	}
	if reason := dockerUnavailable(); reason != "" {
		skipReason = "no TEST_DATABASE_URL and Docker is unavailable (" + reason + ")"
		return
	}
	ctx := context.Background()
	c, err := postgres.RunContainer(ctx,
		testcontainers.WithImage(image),
		postgres.WithDatabase("qa_test"),
		postgres.WithUsername("qa"),
		postgres.WithPassword("qa_test_password"),
		testcontainers.WithWaitStrategy(
			// Postgres logs this twice: once for the init-script server, once for the real one.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		startErr = err
		return
	}
	container = c
	dsn, startErr = c.ConnectionString(ctx, "sslmode=disable")
}

// dockerUnavailable returns why Docker cannot be used, or "" if it can.
// Testcontainers panics when it cannot locate a Docker host, hence the recover.
func dockerUnavailable() (reason string) {
	defer func() {
		if r := recover(); r != nil {
			reason = fmt.Sprint(r)
		}
	}()
	p, err := testcontainers.NewDockerProvider()
	if err != nil {
		return err.Error()
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Health(ctx); err != nil {
		return err.Error()
	}
	return ""
}

func open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(60)
	t.Cleanup(func() { db.Close() })
	return db
}

// DB returns a connection to the migrated test database with all queue tables
// emptied. Users and audit logs persist across tests (the audit table is
// append-only by design), so tests should use unique emails.
func DB(t *testing.T) *sql.DB {
	t.Helper()
	db := open(t, DSN(t))
	migrateOnce.Do(func() {
		goose.SetLogger(goose.NopLogger())
		migrateErr = migrations.Up(db)
	})
	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	if _, err := db.Exec(`TRUNCATE "qa-queue_events", "qa-appointments", "qa-tickets", "qa-ticket_sequences",
		"qa-idempotency_keys", "qa-counters", "qa-services", "qa-branches" CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

// NewDatabase creates an empty, unmigrated database on the test server and
// returns its DSN; it is dropped when the test ends.
func NewDatabase(t *testing.T) string {
	t.Helper()
	base := DSN(t)
	admin := open(t, base)

	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := "qa_iso_" + hex.EncodeToString(b)
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
