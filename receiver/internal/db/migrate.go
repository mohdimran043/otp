package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one versioned schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string

	// Checksum is the SHA-256 of the statements, recorded when the migration is
	// applied. See Migrate for what it is for.
	Checksum string
}

// ErrMigrationChanged means a migration already applied to this database no longer
// matches the one in the binary.
var ErrMigrationChanged = errors.New("db: an applied migration has been modified")

// Migrations returns every migration the binary carries, in version order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}

	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}

		// Names are NNN_description.sql, so the version is explicit and the ordering
		// cannot depend on how a filesystem happens to sort.
		parts := strings.SplitN(strings.TrimSuffix(e.Name(), ".sql"), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("db: migration %q is not named NNN_description.sql", e.Name())
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("db: migration %q has no version number: %w", e.Name(), err)
		}

		body, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     parts[1],
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	for i, m := range out {
		if i > 0 && out[i-1].Version == m.Version {
			return nil, fmt.Errorf("db: two migrations share version %d", m.Version)
		}
	}
	return out, nil
}

// Migrate applies every migration this binary carries that the database has not seen.
//
// Two properties are worth stating because they are what make this safe to run at
// startup on every instance:
//
// Each migration runs inside its own transaction, so a failure leaves the schema at
// the last complete version rather than halfway through a change. Postgres supports
// transactional DDL, which is what makes that possible at all — on a database that did
// not, this would need a very different design.
//
// Concurrent instances are serialised by an advisory lock rather than by hoping they
// start at different times. Without it, two replicas booting together would both see
// the same pending migration and both try to create the same table, and the loser's
// error would take down an instance for no reason.
func (p *Pool) Migrate(ctx context.Context) ([]Migration, error) {
	migrations, err := Migrations()
	if err != nil {
		return nil, err
	}

	conn, err := p.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	// A fixed key, chosen once and never changed: two instances have to pick the same
	// number to exclude each other.
	const lockKey = 8734159021
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return nil, fmt.Errorf("db: could not take the migration lock: %w", err)
	}
	defer func() {
		// Best effort: if this fails the connection is being torn down anyway, and the
		// lock is released when the session ends.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     integer     PRIMARY KEY,
			name        text        NOT NULL,
			checksum    text        NOT NULL,
			applied_at  timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("db: could not create the migration table: %w", err)
	}

	rows, err := conn.Query(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	applied := map[int]Migration{}
	for rows.Next() {
		var m Migration
		if err := rows.Scan(&m.Version, &m.Name, &m.Checksum); err != nil {
			rows.Close()
			return nil, err
		}
		applied[m.Version] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var ran []Migration
	for _, m := range migrations {
		if prev, done := applied[m.Version]; done {
			// A migration that has already run must not have changed. Editing one in place
			// produces a database whose schema does not match what the code believes, and
			// the mismatch surfaces as inexplicable query errors much later. The fix is
			// always a new migration, never an edit to an old one.
			if prev.Checksum != m.Checksum {
				return nil, fmt.Errorf("%w: %03d_%s was applied as %s and is now %s; add a new migration instead",
					ErrMigrationChanged, m.Version, m.Name, prev.Checksum[:12], m.Checksum[:12])
			}
			continue
		}

		if err := applyOne(ctx, conn, m); err != nil {
			return ran, err
		}
		ran = append(ran, m)
	}
	return ran, nil
}

// applyOne runs a single migration in its own transaction.
func applyOne(ctx context.Context, conn *pgxpool.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: %03d_%s: %w", m.Version, m.Name, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("db: %03d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("db: %03d_%s: recording it: %w", m.Version, m.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: %03d_%s: %w", m.Version, m.Name, err)
	}
	return nil
}

// SchemaVersion returns the highest applied migration version, or zero on an empty
// database.
func (p *Pool) SchemaVersion(ctx context.Context) (int, error) {
	var version *int
	err := p.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		// An empty database has no migration table at all, which is version zero rather
		// than an error.
		if strings.Contains(err.Error(), "does not exist") {
			return 0, nil
		}
		return 0, err
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}

// AppliedAt returns when each migration ran, for the diagnostics endpoint.
func (p *Pool) AppliedAt(ctx context.Context) (map[int]time.Time, error) {
	rows, err := p.Query(ctx, `SELECT version, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int]time.Time{}
	for rows.Next() {
		var version int
		var at time.Time
		if err := rows.Scan(&version, &at); err != nil {
			return nil, err
		}
		out[version] = at
	}
	return out, rows.Err()
}
