// Package db opens SQLite and applies embedded, numbered migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/17xande-dev/outbox/sqlitestore"
	"github.com/17xande-dev/sqlitekit"
	"github.com/17xande-dev/sqlitekit/goosestrict"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// dbOptions are the choices sqlitekit leaves to the app: leave deletes nothing
// that must be scrubbed from free pages, so the defaults stand.
var dbOptions = sqlitekit.Options{}

// checkSettings is a seam: a test swaps it for a failing function to prove
// that opening the database runs the check and refuses to start on failure.
var checkSettings = sqlitekit.Check

// Open opens (creating if needed) the database and applies embedded migrations.
func Open(path string) (*sql.DB, error) {
	release, err := lockDatabase(path)
	if err != nil {
		return nil, err
	}
	defer release()
	d, p, err := openProvider(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := migrate(ctx, d, p); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// lockMigrations is a seam only so a test can prove the race the lock closes.
var lockMigrations = func(ctx context.Context, path string) (func() error, error) {
	return sqlitekit.MigrationLock(ctx, path)
}

// lockDatabase takes sqlitekit's cross-process migration lock for the database at
// path. Every caller of openProvider takes it first and holds it until it has
// finished with the schema: the lock must precede the first query (the first
// connection of a fresh file is not covered by busy_timeout), and even Status
// writes, since Check switches the file to WAL.
func lockDatabase(path string) (release func() error, err error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	release, err = lockMigrations(ctx, abs)
	if err != nil {
		return nil, fmt.Errorf("lock database for migration: %w", err)
	}
	return release, nil
}

// openProvider opens the database and builds the migration provider. The caller
// holds the lock from lockDatabase.
func openProvider(path string) (*sql.DB, *goose.Provider, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	// sqlitekit.DSN turns on foreign keys, WAL, a busy timeout and
	// _txlock=immediate. The last one is what lets two concurrent approvals
	// serialise at BEGIN instead of both reading "pending" and one failing with
	// SQLITE_BUSY at commit; see leave's withTx.
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve database path: %w", err)
	}
	dsn, err := sqlitekit.DSN(abs, dbOptions)
	if err != nil {
		return nil, nil, err
	}
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (*sql.DB, *goose.Provider, error) { d.Close(); return nil, nil, e }
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Both drivers silently ignore a misspelled parameter: verify what took.
	if err := checkSettings(ctx, d, dbOptions); err != nil {
		return fail(fmt.Errorf("sqlite settings: %w", err))
	}
	var existing, versioned bool
	if err = d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'goose_db_version'),EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='goose_db_version')`).Scan(&existing, &versioned); err != nil {
		return fail(err)
	}
	if existing && !versioned {
		return fail(fmt.Errorf("database predates migrations: back it up; use make recreate-db only for local development, or explicitly adopt the verified current schema at migration 1 for production"))
	}
	source, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fail(err)
	}
	// goosestrict is goose on a STRICT version table. It also refuses a
	// database whose version is newer than this build's last migration.
	p, err := goosestrict.NewProvider(d, source)
	if err != nil {
		return fail(err)
	}
	return d, p, nil
}

// Status reports pending/applied migrations without applying pending migrations.
func Status(ctx context.Context, path string, w io.Writer) error {
	release, err := lockDatabase(path)
	if err != nil {
		return err
	}
	defer release()
	d, p, err := openProvider(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return writeStatus(ctx, d, p, w)
}

// Migrate applies pending migrations with the same safeguards as Open, then
// reports the resolved path, version change and status. The file must already
// exist so a typo cannot silently migrate a new database instead of the copy.
// It does not start the application or load its config.
func Migrate(ctx context.Context, path string, w io.Writer) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("refusing to migrate database %s: file must already exist: %w", path, err)
	}
	release, err := lockDatabase(path)
	if err != nil {
		return err
	}
	defer release()
	d, p, err := openProvider(path)
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	before, _, err := p.GetVersions(ctx)
	if err != nil {
		return err
	}
	if err := migrate(ctx, d, p); err != nil {
		return err
	}
	after, _, err := p.GetVersions(ctx)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "migrating %s: %d -> %d\n", path, before, after); err != nil {
		return err
	}
	return writeStatus(ctx, d, p, w)
}

// Both entry points hold the same migration lock through the import. On a
// fresh DB the app must create its goose history before any module tables.
func migrate(ctx context.Context, d *sql.DB, p *goose.Provider) error {
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply database migrations: %w", err)
	}
	if err := sqlitestore.Migrate(ctx, d); err != nil {
		return fmt.Errorf("apply outbox migrations: %w", err)
	}
	return importLegacyOutbox(ctx, d)
}

func writeStatus(ctx context.Context, d *sql.DB, p *goose.Provider, w io.Writer) error {
	statuses, err := p.Status(ctx)
	if err != nil {
		return err
	}
	for _, s := range statuses {
		if _, err = fmt.Fprintf(w, "%04d %s %s\n", s.Source.Version, s.State, filepath.Base(s.Source.Path)); err != nil {
			return err
		}
	}
	return writeOutboxStatus(ctx, d, w)
}
