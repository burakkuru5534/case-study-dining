// Package migrate is a small forward-only SQL migration runner.
//
// Guarantees:
//   - Migrations are applied in version order; versions must be contiguous from 1.
//   - Each migration and its schema_migrations row commit in one transaction,
//     so a migration is either fully applied or not at all.
//   - A session-level advisory lock, held on the single connection that also runs
//     the migrations, serializes concurrent runners (e.g. several instances starting).
//
// Migrations must be transactional SQL; statements that cannot run inside a
// transaction (e.g. CREATE INDEX CONCURRENTLY) are not supported.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// lockKey identifies the migration lock among all advisory locks in the database.
// Arbitrary but fixed: every runner of this application must use the same value.
const lockKey int64 = 0x64696e696e67 // "dining"

// unlockTimeout bounds the explicit unlock; if it fails, closing the connection
// ends the session and PostgreSQL releases the lock anyway.
const unlockTimeout = 5 * time.Second

var fileNamePattern = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.sql$`)

// Migration is a single versioned SQL script.
type Migration struct {
	Version int64
	Name    string
	SQL     string
}

// Load reads *.sql files from the root of fsys, sorted by version.
// Files must be named <version>_<name>.sql (e.g. 0001_create_orders.sql).
// Versions must start at 1 and be contiguous; duplicates and gaps are errors.
// Non-.sql files are ignored.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var migrations []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := fileNamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("invalid migration file name %q: want <version>_<name>.sql", e.Name())
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid migration version in %q: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}
		migrations = append(migrations, Migration{Version: version, Name: m[2], SQL: string(body)})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })

	if err := validate(migrations); err != nil {
		return nil, err
	}
	return migrations, nil
}

// validate checks that migrations are sorted, unique and contiguous from 1.
func validate(migrations []Migration) error {
	for i, m := range migrations {
		want := int64(i + 1)
		switch {
		case i > 0 && m.Version == migrations[i-1].Version:
			return fmt.Errorf("duplicate migration version %d", m.Version)
		case m.Version != want:
			return fmt.Errorf("migration version gap: expected %d, found %d (%s)", want, m.Version, m.Name)
		}
	}
	return nil
}

// Run applies all pending migrations on conn and returns how many were applied.
//
// conn must be a dedicated connection (not borrowed from a pool) that the caller
// closes afterwards: the advisory lock belongs to this session.
func Run(ctx context.Context, conn *pgx.Conn, migrations []Migration, logger *slog.Logger) (applied int, err error) {
	// Checked here too, so a hand-built slice cannot bypass Load's rules.
	if err := validate(migrations); err != nil {
		return 0, err
	}

	// Blocks until the lock is free; ctx cancellation aborts the wait.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return 0, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
		defer cancel()
		if _, uerr := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey); uerr != nil {
			err = errors.Join(err, fmt.Errorf("release migration lock: %w", uerr))
		}
	}()

	// Created under the lock, so concurrent runners cannot race on it.
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    bigint      PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}

	appliedVersions, err := loadApplied(ctx, conn)
	if err != nil {
		return 0, err
	}

	// Applied versions must be a prefix of the known migrations. Anything else
	// means the database is ahead of this binary or history was rewritten.
	if len(appliedVersions) > len(migrations) {
		return 0, fmt.Errorf("database has %d applied migrations but only %d are known: binary is older than the database",
			len(appliedVersions), len(migrations))
	}
	for i, v := range appliedVersions {
		if v != migrations[i].Version {
			return 0, fmt.Errorf("applied migration history does not match: position %d is version %d in database, %d in files",
				i, v, migrations[i].Version)
		}
	}

	for _, m := range migrations[len(appliedVersions):] {
		start := time.Now()
		if err := applyOne(ctx, conn, m); err != nil {
			return applied, fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
		}
		applied++
		logger.Info("migration applied", "version", m.Version, "name", m.Name, "duration", time.Since(start))
	}
	return applied, nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// No-op after a successful Commit.
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Exec without arguments uses the simple protocol, so a file may contain
	// multiple statements.
	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func loadApplied(ctx context.Context, conn *pgx.Conn) ([]int64, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return versions, nil
}
