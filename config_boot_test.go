package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInvalidBaseURLStopsBeforeDatabase(t *testing.T) {
	for _, mode := range []string{"server", "import2026"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "leave.db")
			t.Setenv("DB_PATH", path)
			t.Setenv("DIRECTORY", "fake")
			t.Setenv("COOKIE_SECURE", "false")
			t.Setenv("BASE_URL", "http://localhost:8082/not-an-origin")

			if mode == "import2026" {
				var output bytes.Buffer
				args := []string{"-leave=" + fixtureLeave, "-balances=" + fixtureBal, "-recorded-by=alex", "-apply"}
				if code := runImport2026(args, &output, time.Now); code != 2 || !strings.Contains(output.String(), "BASE_URL:") {
					t.Error("import2026 did not reject BASE_URL")
				}
			} else if err := run(); err == nil || !strings.Contains(err.Error(), "BASE_URL:") {
				t.Errorf("server did not reject BASE_URL: %v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid BASE_URL reached the database: stat error = %v", err)
			}
		})
	}
}
