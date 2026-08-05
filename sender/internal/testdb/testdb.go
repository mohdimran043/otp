// Package testdb gives every test its own Postgres database.
//
// Sharing one database between test packages does not work, and the way it fails is
// misleading: `go test ./...` runs packages in parallel, so a package that resets the
// schema pulls the tables out from under another package's running test, which then fails
// with "relation does not exist" — an error that looks like a bug in the code under test
// rather than in the test setup.
//
// Each database is cloned from a template that has already been migrated, which is what
// makes per-test isolation affordable. Creating a database from a template is a file copy
// inside Postgres; running the migrations again for every test would cost far more, and it
// would mean every test paid for a schema it did not change.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/db"
)

// templateName is the migrated database every test database is copied from.
const templateName = "otp_test_template"

var templateOnce sync.Once
var templateErr error

// URL returns the administrative connection string, or skips the test.
//
// Skipping rather than failing is deliberate: `go test ./...` should work on a machine with
// no database, and the Makefile targets that start the container set the variable. A skipped
// test is visible in the output; one that silently passed without a database would not be.
func URL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("OTP_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("set OTP_TEST_DATABASE_URL to run the database integration tests")
	}
	return u
}

// New returns a migrated pool on a database of its own, dropped when the test ends.
func New(t *testing.T) *db.Pool {
	t.Helper()
	ctx := context.Background()
	admin := URL(t)

	templateOnce.Do(func() { templateErr = ensureTemplate(ctx, admin) })
	if templateErr != nil {
		t.Fatalf("testdb: could not prepare the template database: %v", templateErr)
	}

	// A name derived from the test would be friendlier in a `\l` listing, but test names
	// contain characters an identifier cannot, and truncating them collides. A fresh
	// identifier cannot.
	name := "otp_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	if err := exec(ctx, admin, fmt.Sprintf(
		`CREATE DATABASE %s TEMPLATE %s`, quoteIdent(name), quoteIdent(templateName))); err != nil {
		t.Fatalf("testdb: could not create %s: %v", name, err)
	}

	cfg := config.Default().Database
	cfg.URL = replaceDatabase(admin, name)
	// A test needs a couple of connections, not sixteen, and a hundred tests each holding
	// sixteen would exhaust the server's connection limit.
	cfg.MaxConns = 6
	cfg.MinConns = 0

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("testdb: could not open %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		// FORCE terminates anything still connected — a goroutine the test left running, say
		// — because otherwise the drop fails and the database leaks for the rest of the
		// session.
		if err := exec(context.Background(), admin,
			fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, quoteIdent(name))); err != nil {
			t.Logf("testdb: could not drop %s: %v", name, err)
		}
	})
	return pool
}

// URLFor returns the connection string of a fresh database, for tests that need to hand a
// URL to something rather than use a pool directly.
func URLFor(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("testdb: could not read the database name: %v", err)
	}
	return replaceDatabase(URL(t), name)
}

// ensureTemplate creates and migrates the template database if it is not already there.
//
// Concurrent test binaries race here, so creation is serialised by an advisory lock on the
// administrative database and a duplicate is treated as success. Without the lock, two
// packages starting together would both create the template and one would fail on an error
// that says nothing about the code being tested.
func ensureTemplate(ctx context.Context, admin string) error {
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	const lockKey = 5512094418
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return err
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKey)

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, templateName).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}

	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, quoteIdent(templateName))); err != nil {
		// 42P04 is duplicate_database: another binary won the race between the check and the
		// create, which is the outcome this function wants anyway.
		if !strings.Contains(err.Error(), "42P04") && !strings.Contains(err.Error(), "already exists") {
			return err
		}
		return nil
	}

	// Migrate it, then disconnect: Postgres refuses to copy a template that has sessions
	// connected to it.
	cfg := config.Default().Database
	cfg.URL = replaceDatabase(admin, templateName)
	cfg.MaxConns = 2
	cfg.MinConns = 0

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	_, err = pool.Migrate(ctx)
	pool.Close()
	return err
}

func exec(ctx context.Context, dsn, statement string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, statement)
	return err
}

// replaceDatabase swaps the database name in a connection string, keeping everything else.
func replaceDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// quoteIdent quotes an identifier for use in DDL, which cannot be parameterised.
//
// The names here are generated rather than supplied, so this is belt and braces — but a
// helper that builds DDL by string concatenation should quote regardless, because the next
// caller may not be so careful about where its name came from.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
