package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/config"
	"github.com/17xande-dev/leave/internal/dbtest"
)

// The config becomes the backup layout the README documents; KEEP=0 is off.
func TestBackupsWiring(t *testing.T) {
	b, ok := backups(config.Config{BackupDir: "/d", BackupKeep: 14})
	if !ok || b.Dir != "/d" || b.Prefix != "leave-" || b.Layout != "2006-01-02" || b.Keep != 14 || b.Location != time.UTC {
		t.Fatalf("backups = %+v, ok = %v", b, ok)
	}
	if _, ok := backups(config.Config{BackupDir: "/d", BackupKeep: 0}); ok {
		t.Fatal("BACKUP_KEEP=0 did not turn backups off")
	}
}

func TestBackupJobWritesTodaysCopy(t *testing.T) {
	d := dbtest.New(t)
	dir := filepath.Join(t.TempDir(), "backups")
	backupJob(d, config.Config{BackupDir: dir, BackupKeep: 3})(context.Background())
	name := "leave-" + time.Now().UTC().Format("2006-01-02") + ".db"
	if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Size() == 0 {
		t.Fatalf("backup %s: %v", name, err)
	}

	off := t.TempDir()
	backupJob(d, config.Config{BackupDir: filepath.Join(off, "b"), BackupKeep: 0})(context.Background())
	if _, err := os.Stat(filepath.Join(off, "b")); err == nil {
		t.Fatal("backups off, yet the folder was created")
	}
}

// The loop is started by the real startup path: serve, the function main
// calls, must leave today's backup in BACKUP_DIR without anyone asking.
func TestServeStartsBackups(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DIRECTORY", "fake")
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("LISTEN_ADDR", "127.0.0.1:0")
	// The CSRF layer needs the app's own origin, so serve refuses to start without one.
	t.Setenv("BASE_URL", "http://localhost:8082")
	t.Setenv("DB_PATH", filepath.Join(root, "leave.db"))
	t.Setenv("BACKUP_DIR", filepath.Join(root, "bk"))
	t.Setenv("BACKUP_KEEP", "5")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg) }()

	want := filepath.Join(root, "bk", "leave-"+time.Now().UTC().Format("2006-01-02")+".db")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(want); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("serve returned early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("serve started no backup: %s missing", want)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve after cancel: %v", err)
	}
}

// A BASE_URL the CSRF layer would refuse must stop the program in
// config.Load, before the database is opened: db.Open creates and migrates the
// file, and a typo should not leave a migrated database behind.
func TestBadBaseURLStopsBeforeDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leave.db")
	t.Setenv("DIRECTORY", "fake")
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("BASE_URL", "http://localhost:08082")
	t.Setenv("DB_PATH", path)
	err := run()
	if err == nil || !strings.Contains(err.Error(), "BASE_URL") {
		t.Fatalf("run() = %v, want a BASE_URL error", err)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s exists: the database was opened before BASE_URL was checked", f)
		}
	}
}
