package config_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bangdoni/container-watch/internal/config"
)

// clearEnv removes every variable the loader reads so a test starts from a
// known state. t.Setenv restores the previous value on cleanup, so the
// order here does not matter.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CW_ADDR", "CW_DATA_DIR", "CW_DATABASE_PATH", "CW_PUBLIC_URL",
		"CW_SESSION_TTL", "CW_METRICS_INTERVAL", "CW_METRICS_WINDOW",
		"CW_SECURE_COOKIES", "CW_TRUST_PROXY", "CW_LOCAL_SOCKETS",
		"CW_LOG_LEVEL", "CW_PUBLIC_HEALTHCHECK",
	} {
		t.Setenv(k, "")
	}
}

// TestLoadDefaults checks that an empty environment yields the documented
// defaults.
func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr == "" {
		t.Error("Addr is empty")
	}
	if c.DatabasePath == "" {
		t.Error("DatabasePath is empty")
	}
	if c.SessionTTL <= 0 {
		t.Errorf("SessionTTL = %v, want a positive duration", c.SessionTTL)
	}
	if c.MetricsInterval < 250*time.Millisecond {
		t.Errorf("MetricsInterval = %v, want at least 250ms", c.MetricsInterval)
	}
	if c.MetricsWindow < 5 {
		t.Errorf("MetricsWindow = %d, want at least 5", c.MetricsWindow)
	}
}

// TestLoadReadsEnvironment checks the variable mapping.
func TestLoadReadsEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("CW_ADDR", "127.0.0.1:9999")
	dataDir := t.TempDir()
	t.Setenv("CW_DATA_DIR", dataDir)
	t.Setenv("CW_PUBLIC_URL", "https://containers.example.com/")
	t.Setenv("CW_SESSION_TTL", "2h")
	t.Setenv("CW_METRICS_INTERVAL", "5s")
	t.Setenv("CW_METRICS_WINDOW", "120")
	t.Setenv("CW_TRUST_PROXY", "true")
	t.Setenv("CW_SECURE_COOKIES", "true")
	t.Setenv("CW_LOCAL_SOCKETS", "docker:///var/run/docker.sock, podman:///run/podman/podman.sock")

	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q", c.Addr)
	}
	if c.DataDir != dataDir {
		t.Errorf("DataDir = %q, want %q", c.DataDir, dataDir)
	}
	// The trailing slash is trimmed so URL joins do not double up.
	if c.PublicURL != "https://containers.example.com" {
		t.Errorf("PublicURL = %q, want the trailing slash trimmed", c.PublicURL)
	}
	if c.SessionTTL != 2*time.Hour {
		t.Errorf("SessionTTL = %v, want 2h", c.SessionTTL)
	}
	if c.MetricsInterval != 5*time.Second {
		t.Errorf("MetricsInterval = %v, want 5s", c.MetricsInterval)
	}
	if c.MetricsWindow != 120 {
		t.Errorf("MetricsWindow = %d, want 120", c.MetricsWindow)
	}
	if !c.TrustProxy {
		t.Error("TrustProxy is false")
	}
	if c.SecureCookies == nil || !*c.SecureCookies {
		t.Error("SecureCookies is not true")
	}
	if len(c.LocalSockets) != 2 {
		t.Errorf("LocalSockets = %v, want 2 entries", c.LocalSockets)
	}
}

// TestLoadDatabasePathDefaultsUnderDataDir checks that the database path
// follows the data directory when it is not set explicitly.
//
// Load() creates the data directory (0700), so the test points it at a
// temp dir rather than a system path.
func TestLoadDatabasePathDefaultsUnderDataDir(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	t.Setenv("CW_DATA_DIR", dir)
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(dir, "containerwatch.db")
	if c.DatabasePath != want {
		t.Errorf("DatabasePath = %q, want %q", c.DatabasePath, want)
	}
}

// TestLoadExplicitDatabasePathWins checks the override.
func TestLoadExplicitDatabasePathWins(t *testing.T) {
	clearEnv(t)
	t.Setenv("CW_DATA_DIR", t.TempDir())
	explicit := filepath.Join(t.TempDir(), "cw.db")
	t.Setenv("CW_DATABASE_PATH", explicit)
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DatabasePath != explicit {
		t.Errorf("DatabasePath = %q, want %q", c.DatabasePath, explicit)
	}
}

// TestLoadRejectsInvalidValues checks that bad input fails loudly rather
// than silently falling back to a default that an operator did not ask for.
func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad session ttl", "CW_SESSION_TTL", "not-a-duration"},
		{"bad metrics interval", "CW_METRICS_INTERVAL", "abc"},
		{"metrics interval too small", "CW_METRICS_INTERVAL", "10ms"},
		{"bad metrics window", "CW_METRICS_WINDOW", "abc"},
		{"metrics window too small", "CW_METRICS_WINDOW", "1"},
		{"metrics window too large", "CW_METRICS_WINDOW", "99999"},
		{"bad secure cookies", "CW_SECURE_COOKIES", "maybe"},
		{"bad trust proxy", "CW_TRUST_PROXY", "perhaps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tc.key, tc.val)
			if _, err := config.Load(); err == nil {
				t.Errorf("Load accepted %s=%q", tc.key, tc.val)
			}
		})
	}
}

// TestLoadLocalSocketsNoneDisablesDiscovery checks the "none" sentinel.
func TestLoadLocalSocketsNoneDisablesDiscovery(t *testing.T) {
	clearEnv(t)
	t.Setenv("CW_LOCAL_SOCKETS", "none")
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.DisableLocalDiscovery {
		t.Error("DisableLocalDiscovery is false for CW_LOCAL_SOCKETS=none")
	}
	if len(c.LocalSockets) != 0 {
		t.Errorf("LocalSockets = %v, want empty", c.LocalSockets)
	}
}

// TestLoadLocalSocketsSkipsBlanks checks that stray commas do not create
// empty socket entries.
func TestLoadLocalSocketsSkipsBlanks(t *testing.T) {
	clearEnv(t)
	t.Setenv("CW_LOCAL_SOCKETS", " docker:///a.sock , , podman:///b.sock ,")
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.LocalSockets) != 2 {
		t.Errorf("LocalSockets = %v, want 2 entries", c.LocalSockets)
	}
	for _, s := range c.LocalSockets {
		if s == "" {
			t.Error("LocalSockets contains an empty entry")
		}
	}
}

// TestLoadRejectsBadLogLevel checks the log level parsing.
func TestLoadRejectsBadLogLevel(t *testing.T) {
	clearEnv(t)
	t.Setenv("CW_LOG_LEVEL", "verbose")
	if _, err := config.Load(); err == nil {
		t.Error("Load accepted CW_LOG_LEVEL=verbose")
	}
}

// TestPublicHealthcheckDefaultsToLocal checks that /healthz is not exposed to
// remote callers unless the operator opts in.
func TestPublicHealthcheckDefaultsToLocal(t *testing.T) {
	clearEnv(t)
	t.Setenv("CW_DATA_DIR", t.TempDir())
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PublicHealthcheck {
		t.Error("PublicHealthcheck is true by default; the probe should be local-only")
	}
}

// TestPublicHealthcheckReadsEnvironment checks the variable mapping and that
// a malformed value is rejected rather than silently ignored.
func TestPublicHealthcheckReadsEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("CW_DATA_DIR", t.TempDir())
	t.Setenv("CW_PUBLIC_HEALTHCHECK", "true")
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.PublicHealthcheck {
		t.Error("PublicHealthcheck = false, want true")
	}

	t.Setenv("CW_PUBLIC_HEALTHCHECK", "not-a-bool")
	if _, err := config.Load(); err == nil {
		t.Error("a malformed CW_PUBLIC_HEALTHCHECK was accepted")
	}
}
