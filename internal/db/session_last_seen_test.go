package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Migration 0010 on a copy of a v9 database that has rows in every table
// (the contract fixture's) plus sessions: the sessions survive and are marked
// seen at the migration, nothing else changes, Down restores the v9 schema and
// rows, and a second run is a no-op.
func TestSessionLastSeenMigration(t *testing.T) {
	ctx := context.Background()
	d, p, _ := contractFixture(t)
	if _, err := p.UpTo(ctx, 9); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("live-token"))
	live := hex.EncodeToString(sum[:])
	created := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	contractExec(t, d, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, 1, ?, ?), ('expired-hash', 1, '2020-01-01T00:00:00Z', '2020-01-02T00:00:00Z')`, live, created, expires)

	// sessions gains a column, so it is compared by the columns it had.
	const oldSessions = `SELECT token_hash, user_id, created_at, expires_at FROM sessions ORDER BY token_hash`
	hashes := func() map[string][32]byte {
		h := contractHashes(t, d, false)
		delete(h, "sessions")
		return h
	}
	before, beforeSessions, schema := hashes(), contractRows(t, d, oldSessions), contractSchema(t, d)
	path := filepath.Join(t.TempDir(), "v9.db")
	contractExec(t, d, `VACUUM INTO ?`, path)

	start := time.Now().UTC().Add(-time.Second)
	if _, err := p.UpTo(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, hashes()) || !reflect.DeepEqual(beforeSessions, contractRows(t, d, oldSessions)) {
		t.Fatal("0010 changed existing application data")
	}
	for _, r := range contractRows(t, d, `SELECT token_hash, last_seen FROM sessions`) {
		seen, err := time.Parse(time.RFC3339, r[1].(string))
		if err != nil || seen.Before(start) || seen.After(time.Now().Add(time.Second)) {
			t.Fatalf("%v: last_seen not backfilled to the migration time: %q (%v)", r[0], r[1], err)
		}
	}

	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err := p.GetDBVersion(ctx); err != nil || version != 9 {
		t.Fatalf("Down version = %d (%v), want 9", version, err)
	}
	if !reflect.DeepEqual(schema, contractSchema(t, d)) || !reflect.DeepEqual(before, hashes()) || !reflect.DeepEqual(beforeSessions, contractRows(t, d, oldSessions)) {
		t.Fatal("Down did not restore the v9 schema and data")
	}
	// The previous binary's insert names its columns, and works on v9 again.
	contractExec(t, d, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ('old-binary', 1, 'now', 'later')`)
	contractExec(t, d, `DELETE FROM sessions WHERE token_hash = 'old-binary'`)
	if _, err := p.UpTo(ctx, 10); err != nil {
		t.Fatal(err)
	}

	// The operator's undo script (README rollback) must leave what Down leaves:
	// the v9 schema, the old columns' rows and goose at version 9.
	script, err := os.ReadFile("testdata/undo_0010.sql")
	if err != nil {
		t.Fatal(err)
	}
	contractExec(t, d, string(script))
	if version, err := p.GetDBVersion(ctx); err != nil || version != 9 {
		t.Fatalf("undo_0010 version = %d (%v), want 9", version, err)
	}
	if !reflect.DeepEqual(schema, contractSchema(t, d)) || !reflect.DeepEqual(before, hashes()) || !reflect.DeepEqual(beforeSessions, contractRows(t, d, oldSessions)) {
		t.Fatal("undo_0010 did not restore the v9 schema and data")
	}
	// And it is Down, statement for statement, plus the version row.
	src, err := migrations.ReadFile("migrations/0010_session_last_seen.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, _ := strings.Cut(string(src), "-- +goose Down\n")
	body := strings.TrimSuffix(strings.TrimPrefix(string(script), "BEGIN IMMEDIATE;\n"), "DELETE FROM goose_db_version WHERE version_id = 10;\nCOMMIT;\n")
	if strings.TrimSpace(body) != strings.TrimSpace(down) {
		t.Fatalf("undo_0010 differs from Down:\n%s\nvs\n%s", body, down)
	}
	if _, err := p.UpTo(ctx, 10); err != nil {
		t.Fatal(err)
	}

	// The real command, twice, on the v9 copy: 9 -> 10, then nothing to do.
	binary := filepath.Join(t.TempDir(), "leave")
	if out, err := exec.Command("go", "build", "-o", binary, "../..").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, want := range []string{"9 -> 10", "10 -> 10"} {
		cmd := exec.Command(binary, "-migrate")
		cmd.Env = append(os.Environ(), "DB_PATH="+path)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), ": "+want+"\n") || !strings.Contains(string(out), "0010 applied 0010_session_last_seen.sql") {
			t.Fatalf("migrate %s: %v\n%s", want, err, out)
		}
	}
}
