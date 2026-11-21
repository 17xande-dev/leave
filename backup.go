package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/17xande-dev/leave/internal/config"
	"github.com/17xande-dev/sqlitekit"
)

// backups is the sqlitekit configuration for leave's daily backups: one
// VACUUM INTO copy per UTC day, named leave-2006-01-02.db, the newest
// cfg.BackupKeep kept. ok is false when BACKUP_KEEP=0 turns backups off.
func backups(cfg config.Config) (b sqlitekit.Backups, ok bool) {
	if cfg.BackupKeep < 1 {
		return sqlitekit.Backups{}, false
	}
	return sqlitekit.Backups{
		Dir:      cfg.BackupDir,
		Prefix:   "leave-",
		Layout:   "2006-01-02",
		Keep:     cfg.BackupKeep,
		Location: time.UTC,
	}, true
}

// backupJob returns the function the hourly background loop calls. It never
// stops the app: a failed backup is logged and tried again next hour.
func backupJob(database *sql.DB, cfg config.Config) func(context.Context) {
	b, ok := backups(cfg)
	if !ok {
		slog.Warn("database backups are off (BACKUP_KEEP=0)")
		return func(context.Context) {}
	}
	slog.Info("database backups", "dir", b.Dir, "keep", b.Keep)
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		path, made, err := b.Run(ctx, database, time.Now())
		if err != nil {
			slog.Error("database backup", "err", err)
			return
		}
		if made {
			slog.Info("database backup written", "path", path)
		}
	}
}
