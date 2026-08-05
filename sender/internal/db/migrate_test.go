package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/config"
	"github.com/opticaltransport/otp/sender/internal/db"
	"github.com/opticaltransport/otp/sender/internal/testdb"
)

// openTestPool returns a pool on a database of this test's own.
//
// These tests reset the schema, which is exactly why each needs its own database: doing it
// on a shared one pulls the tables out from under whatever else is running.
func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	return testdb.New(t)
}

// resetSchema empties the database, so a test that migrates from scratch starts from
// nothing rather than from the template's already-migrated schema.
func resetSchema(t *testing.T, pool *db.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
}

func TestMigrationsApplyToAnEmptyDatabase(t *testing.T) {
	pool := openTestPool(t)
	resetSchema(t, pool)
	ctx := context.Background()

	version, err := pool.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Zero(t, version, "an empty database is at version zero")

	ran, err := pool.Migrate(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, ran)

	all, err := db.Migrations()
	require.NoError(t, err)
	require.Len(t, ran, len(all), "every migration should have run")

	version, err = pool.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, all[len(all)-1].Version, version)

	// Every table the design names must exist, checked by name rather than by trusting
	// that the migration ran without error — a migration can succeed and still have
	// created something other than what the store layer expects.
	for _, table := range []string{
		"files", "chunks", "encoded_frames", "compression_profiles", "encoding_profiles",
		"transmissions", "display_sessions", "callbacks", "jobs", "statistics",
		"protocol_versions", "job_logs", "users", "audit_logs", "schema_migrations",
	} {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
		require.NoError(t, err)
		require.True(t, exists, "table %s is missing", table)
	}
}

// TestMigrationsAreIdempotent is the property that lets every instance run migrations
// at startup: a second run must do nothing rather than fail.
func TestMigrationsAreIdempotent(t *testing.T) {
	pool := openTestPool(t)
	resetSchema(t, pool)
	ctx := context.Background()

	first, err := pool.Migrate(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, first)

	second, err := pool.Migrate(ctx)
	require.NoError(t, err)
	require.Empty(t, second, "a second run has nothing to do")
}

// TestMigrationsRunConcurrently covers two replicas starting together. Without the
// advisory lock both would try to create the same tables and one would die on an error
// that has nothing to do with its own health.
func TestMigrationsRunConcurrently(t *testing.T) {
	pool := openTestPool(t)
	resetSchema(t, pool)
	ctx := context.Background()

	const racers = 4
	errs := make(chan error, racers)
	counts := make(chan int, racers)
	for i := 0; i < racers; i++ {
		go func() {
			ran, err := pool.Migrate(ctx)
			counts <- len(ran)
			errs <- err
		}()
	}

	total := 0
	for i := 0; i < racers; i++ {
		require.NoError(t, <-errs)
		total += <-counts
	}

	all, err := db.Migrations()
	require.NoError(t, err)
	require.Equal(t, len(all), total,
		"each migration must be applied exactly once across all the racers")
}

// TestEditedMigrationIsRefused is the guard against the most damaging thing anybody can
// do to a schema: change a migration that has already run somewhere. The database would
// then be at a state the code does not describe, and the mismatch surfaces much later as
// query errors nobody can trace back.
func TestEditedMigrationIsRefused(t *testing.T) {
	pool := openTestPool(t)
	resetSchema(t, pool)
	ctx := context.Background()

	_, err := pool.Migrate(ctx)
	require.NoError(t, err)

	// Simulate an edit by rewriting the recorded checksum: the effect is the same as the
	// file having changed, and it does not require writing to the embedded filesystem.
	_, err = pool.Exec(ctx,
		`UPDATE schema_migrations SET checksum = repeat('0', 64) WHERE version = 1`)
	require.NoError(t, err)

	_, err = pool.Migrate(ctx)
	require.ErrorIs(t, err, db.ErrMigrationChanged)
}

func TestMigrationsAreWellFormed(t *testing.T) {
	all, err := db.Migrations()
	require.NoError(t, err)
	require.NotEmpty(t, all)

	seen := map[int]bool{}
	for i, m := range all {
		require.NotZero(t, m.Version, "a migration cannot be version zero")
		require.False(t, seen[m.Version], "version %d appears twice", m.Version)
		seen[m.Version] = true
		require.NotEmpty(t, m.Name)
		require.NotEmpty(t, m.SQL)
		require.Len(t, m.Checksum, 64)
		if i > 0 {
			require.Greater(t, m.Version, all[i-1].Version, "migrations must be ordered")
		}
	}
}

// TestUnreachableDatabaseFailsFast covers the startup check. A pool is lazy, so without
// it a wrong URL yields a process that starts, reports itself healthy, and fails on the
// first request an operator makes.
func TestUnreachableDatabaseFailsFast(t *testing.T) {
	cfg := config.Default().Database
	cfg.URL = "postgres://nobody:secret@127.0.0.1:1/nothing?sslmode=disable"
	cfg.ConnectTimeout = 2 * 1000 * 1000 * 1000 // two seconds

	_, err := db.Open(context.Background(), cfg)
	require.Error(t, err)

	// And the error must not carry the password, since an unreachable database is the
	// error most likely to end up pasted into a ticket.
	require.NotContains(t, err.Error(), "secret")
}
