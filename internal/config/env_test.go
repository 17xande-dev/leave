package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/env/envtest"
)

func TestEnvironmentDocumentation(t *testing.T) {
	recorder := envtest.NewRecorder(local())
	if _, err := load(recorder.Lookup); err != nil {
		t.Fatalf("load minimal configuration: %v", err)
	}
	envtest.Documented(t, ".env.example", recorder.Keys(), envtest.EnvExample(t, "../../.env.example"))
}

func TestProductionValues(t *testing.T) {
	values := ldap()
	// env.clear in config/deploy.yml; the LDAP credentials are fixtures.
	values["BASE_URL"] = "https://leave.rivers.tools"
	values["COOKIE_SECURE"] = "true"
	values["DIRECTORY"] = "ldap"
	values["TZ_NAME"] = "Africa/Johannesburg"
	values["TRUSTED_PROXIES"] = "172.30.17.0/24"

	c, err := load(envtest.NewRecorder(values).Lookup)
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://leave.rivers.tools" || !c.CookieSecure || c.Directory.Kind != "ldap" ||
		c.Location.String() != "Africa/Johannesburg" || !slices.Equal(c.TrustedProxies, []string{"172.30.17.0/24"}) {
		t.Fatal("production config did not preserve the deployment settings")
	}
}

func TestConfigAggregatesProblemsWithoutEchoingInput(t *testing.T) {
	values := local()
	const marker = "private-looking-fixture"
	secretPath := filepath.Join(t.TempDir(), marker)
	values["GRAPH_CLIENT_SECRET_FILE"] = secretPath
	values["DIRECTORY_SYNC_INTERVAL"] = marker
	values["DEV_SMTP_PORT"] = marker
	values["TRUSTED_PROXIES"] = marker
	values["TZ_NAME"] = marker

	_, err := load(envtest.NewRecorder(values).Lookup)
	if err == nil {
		t.Fatal("invalid settings were accepted")
	}
	for _, key := range []string{"GRAPH_CLIENT_SECRET_FILE", "DIRECTORY_SYNC_INTERVAL", "DEV_SMTP_PORT", "TRUSTED_PROXIES", "TZ_NAME"} {
		if !strings.Contains(err.Error(), key+":") {
			t.Errorf("error did not identify %s", key)
		}
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), secretPath) {
		t.Fatal("error disclosed a value or secret-file path")
	}
}

func TestConfigRedactsCredentials(t *testing.T) {
	values := ldap()
	const bindPassword, graphSecret = "  fixture-ldap-password  ", "fixture-graph-secret"
	values["LDAP_BIND_PASSWORD"] = bindPassword
	values["GRAPH_CLIENT_SECRET"] = graphSecret
	values["GRAPH_TENANT_ID"] = "tenant-fixture"
	values["GRAPH_CLIENT_ID"] = "app-fixture"
	values["EMAIL_FROM"] = "leave@example.test"
	values["LEAVE_CALENDAR_MAILBOX"] = "calendar@example.test"

	c, err := load(envtest.NewRecorder(values).Lookup)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%+v", "%#v"} {
		out := fmt.Sprintf(format, c)
		if strings.Contains(out, bindPassword) || strings.Contains(out, graphSecret) || strings.Count(out, "[redacted]") != 2 {
			t.Fatal("configuration diagnostics did not redact both credentials")
		}
	}
	if c.Directory.BindPassword.Reveal() != bindPassword || c.Graph.ClientSecret.Reveal() != graphSecret {
		t.Fatal("credential did not reach its consumer unchanged")
	}
}

func TestLegacyPaddedSettings(t *testing.T) {
	values := local()
	values["DIRECTORY"] = " fake "
	values["DIRECTORY_SYNC_INTERVAL"] = " 2h "
	values["DEV_SMTP_PORT"] = " 1026 "
	values["BACKUP_KEEP"] = " 3 "
	c, err := load(envtest.NewRecorder(values).Lookup)
	if err != nil || c.Directory.Kind != "fake" || c.Directory.SyncInterval != 2*time.Hour ||
		c.DevSMTP.Port != 1026 || c.BackupKeep != 3 {
		t.Fatalf("padded settings did not load: %v", err)
	}
}
