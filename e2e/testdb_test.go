package e2e_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// uniqueDatabase creates a database of this test's own and returns its connection string.
//
// A database per test rather than a shared one, because these tests run two whole applications and
// several long-running loops. Sharing would mean one test's shutdown racing another's queries, and the
// resulting failures would look like bugs in the code under test rather than in the test setup.
func uniqueDatabase(t *testing.T, admin, role string) string {
	t.Helper()

	name := fmt.Sprintf("otp_e2e_%s_%s", role, strings.ReplaceAll(uuid.NewString(), "-", ""))
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("e2e: could not reach the database server: %v", err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		conn.Close(ctx)
		t.Fatalf("e2e: could not create %s: %v", name, err)
	}
	conn.Close(ctx)

	t.Cleanup(func() {
		clean, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer clean.Close(context.Background())
		// FORCE, because a loop the test left running may still hold a connection; without it the drop
		// fails and the database leaks for the rest of the session.
		_, _ = clean.Exec(context.Background(),
			fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
	})

	return replaceDatabase(admin, name)
}

func replaceDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}
