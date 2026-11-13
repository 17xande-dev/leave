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

	_ "github.com/mattn/go-sqlite3"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens (creating if needed) the database and applies embedded migrations.
func Open(path string) (*sql.DB, error) {
	d, p, err := openProvider(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err = p.Up(ctx); err != nil {
		d.Close()
		return nil, fmt.Errorf("apply database migrations: %w", err)
	}
	return d, nil
}

func openProvider(path string) (*sql.DB, *goose.Provider, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	// _foreign_keys=on because SQLite enforces no foreign key at all by
	// default. busy_timeout is left at go-sqlite3's 5s default. _txlock=immediate
	// makes every transaction take the write lock at BEGIN, so two concurrent
	// approvals serialise instead of both reading "pending" and one failing
	// with SQLITE_BUSY at commit.
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_txlock=immediate", path)
	d, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (*sql.DB, *goose.Provider, error) { d.Close(); return nil, nil, e }
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	base, err := database.NewStore(database.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		return fail(err)
	}
	// Goose's SQLite store uses a TIMESTAMP column, which SQLite STRICT rejects.
	// Retain its version operations and adapt metadata creation/discovery/date decoding.
	p, err := goose.NewProvider(goose.DialectCustom, d, source, goose.WithStore(strictVersionStore{base}))
	if err != nil {
		return fail(err)
	}
	current, target, err := p.GetVersions(ctx)
	if err != nil {
		return fail(err)
	}
	if current > target {
		return fail(fmt.Errorf("database migration version %d is newer than this binary's latest migration %d; use a newer binary", current, target))
	}
	return d, p, nil
}

type strictVersionStore struct{ database.Store }

// Table existence must not depend on version 0 being present: adopted or
// repaired databases can have an applied baseline without that sentinel row.
func (s strictVersionStore) TableExists(ctx context.Context, d database.DBTxConn) (bool, error) {
	var exists bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, s.Tablename()).Scan(&exists)
	return exists, err
}

func (s strictVersionStore) CreateVersionTable(ctx context.Context, d database.DBTxConn) error {
	_, err := d.ExecContext(ctx, `CREATE TABLE goose_db_version(id INTEGER PRIMARY KEY AUTOINCREMENT,version_id INTEGER NOT NULL,is_applied INTEGER NOT NULL,tstamp TEXT NOT NULL DEFAULT (datetime('now'))) STRICT`)
	return err
}
func (s strictVersionStore) GetMigration(ctx context.Context, d database.DBTxConn, version int64) (*database.GetMigrationResult, error) {
	var timestamp string
	var result database.GetMigrationResult
	err := d.QueryRowContext(ctx, `SELECT tstamp,is_applied FROM goose_db_version WHERE version_id=? ORDER BY id DESC LIMIT 1`, version).Scan(&timestamp, &result.IsApplied)
	if err == sql.ErrNoRows {
		return nil, database.ErrVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	result.Timestamp, err = time.Parse("2006-01-02 15:04:05", timestamp)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Status reports pending/applied migrations without applying pending migrations.
func Status(ctx context.Context, path string, w io.Writer) error {
	d, p, err := openProvider(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return writeStatus(ctx, p, w)
}

// Migrate applies pending migrations with the same safeguards as Open, then
// reports their status. It does not start the application or load its config.
func Migrate(ctx context.Context, path string, w io.Writer) error {
	d, p, err := openProvider(path)
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply database migrations: %w", err)
	}
	return writeStatus(ctx, p, w)
}

func writeStatus(ctx context.Context, p *goose.Provider, w io.Writer) error {
	statuses, err := p.Status(ctx)
	if err != nil {
		return err
	}
	for _, s := range statuses {
		if _, err = fmt.Fprintf(w, "%04d %s %s\n", s.Source.Version, s.State, filepath.Base(s.Source.Path)); err != nil {
			return err
		}
	}
	return nil
}
