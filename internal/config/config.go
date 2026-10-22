// Package config loads ContainerWatch's runtime configuration from the
// environment. Every knob has a sensible default so the binary can be started
// with zero configuration on a workstation.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved application configuration.
type Config struct {
	// Addr is the TCP address the HTTP server listens on (e.g. ":8080").
	Addr string
	// DataDir holds the SQLite database and, if no explicit database path is
	// given, the session secret.
	DataDir string
	// DatabasePath is the location of the SQLite database file.
	DatabasePath string
	// PublicURL is the externally reachable base URL. When empty it is
	// derived per-request, which is convenient behind reverse proxies.
	PublicURL string
	// SessionTTL is the absolute lifetime of a login session.
	SessionTTL time.Duration
	// CookieSecret signs the (unused-but-reserved) CSRF cookie namespace.
	CookieSecret []byte
	// SecureCookies forces the Secure attribute on cookies. When nil the value
	// is inferred from the request (TLS or forwarded proto).
	SecureCookies *bool
	// TrustProxy enables interpretation of X-Forwarded-* headers.
	TrustProxy bool
	// LocalSockets is the ordered list of local container engine sockets to
	// probe on boot, formatted as engine://path (engine is "docker" or
	// "podman"). The scheme is a hint only; the engine is detected on connect.
	LocalSockets []string
	// MetricsInterval controls how often the SSE broker samples containers.
	MetricsInterval time.Duration
	// MetricsWindow is the number of samples retained per container for
	// sparklines and charts.
	MetricsWindow int
	// DisableLocalDiscovery skips probing local sockets entirely.
	DisableLocalDiscovery bool
	// LogLevel is the slog level to use.
	LogLevel slog.Level
	// RegistrationOpen is the initial value for the "public registration"
	// setting, used only before the first admin has configured it.
	RegistrationOpen bool
}

// Default returns a configuration populated entirely from defaults.
func Default() *Config {
	return &Config{
		Addr:            ":8080",
		DataDir:         "./data",
		SessionTTL:      24 * time.Hour,
		MetricsInterval: 2 * time.Second,
		MetricsWindow:   60,
		LogLevel:        slog.LevelInfo,
		LocalSockets: []string{
			"docker:///var/run/docker.sock",
			"podman:///run/podman/podman.sock",
		},
	}
}

var errNoSecret = errors.New("session secret unavailable")

// Load resolves configuration from the process environment.
func Load() (*Config, error) {
	c := Default()

	if v := os.Getenv("CW_ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("CW_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("CW_DATABASE_PATH"); v != "" {
		c.DatabasePath = v
	}
	if c.DatabasePath == "" {
		c.DatabasePath = filepath.Join(c.DataDir, "containerwatch.db")
	}
	if v := os.Getenv("CW_PUBLIC_URL"); v != "" {
		c.PublicURL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("CW_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("CW_SESSION_TTL: %w", err)
		}
		c.SessionTTL = d
	}
	if v := os.Getenv("CW_METRICS_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("CW_METRICS_INTERVAL: %w", err)
		}
		if d < 250*time.Millisecond {
			return nil, errors.New("CW_METRICS_INTERVAL must be at least 250ms")
		}
		c.MetricsInterval = d
	}
	if v := os.Getenv("CW_METRICS_WINDOW"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 5 || n > 3600 {
			return nil, errors.New("CW_METRICS_WINDOW must be an integer between 5 and 3600")
		}
		c.MetricsWindow = n
	}
	if v := os.Getenv("CW_SECURE_COOKIES"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("CW_SECURE_COOKIES: %w", err)
		}
		c.SecureCookies = &b
	}
	if v := os.Getenv("CW_TRUST_PROXY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("CW_TRUST_PROXY: %w", err)
		}
		c.TrustProxy = b
	}
	if v := os.Getenv("CW_LOCAL_SOCKETS"); v != "" {
		if v == "none" {
			c.DisableLocalDiscovery = true
			// "none" must also clear the built-in socket list. Leaving
			// the defaults in place meant DisableLocalDiscovery was set
			// but LocalSockets still named /var/run/docker.sock, so
			// anything reading the list directly would still try to
			// reach a local daemon.
			c.LocalSockets = c.LocalSockets[:0]
		} else {
			parts := strings.Split(v, ",")
			c.LocalSockets = c.LocalSockets[:0]
			for _, p := range parts {
				if p = strings.TrimSpace(p); p != "" {
					c.LocalSockets = append(c.LocalSockets, p)
				}
			}
		}
	}
	if v := os.Getenv("CW_LOG_LEVEL"); v != "" {
		lvl, err := parseLevel(v)
		if err != nil {
			return nil, err
		}
		c.LogLevel = lvl
	}
	if v := os.Getenv("CW_OPEN_REGISTRATION"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("CW_OPEN_REGISTRATION: %w", err)
		}
		c.RegistrationOpen = b
	}

	secret, err := loadSecret(c.DataDir)
	if err != nil {
		return nil, err
	}
	c.CookieSecret = secret

	return c, nil
}

// loadSecret reads CW_COOKIE_SECRET, falling back to a persisted random secret
// in the data directory. A stable secret across restarts keeps sessions valid.
func loadSecret(dataDir string) ([]byte, error) {
	if v := os.Getenv("CW_COOKIE_SECRET"); v != "" {
		if len(v) < 16 {
			return nil, errors.New("CW_COOKIE_SECRET must be at least 16 characters")
		}
		return []byte(v), nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read secret: %w", err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, errNoSecret
	}
	enc := []byte(hex.EncodeToString(buf))
	if err := os.WriteFile(path, enc, 0o600); err != nil {
		return nil, fmt.Errorf("write secret: %w", err)
	}
	return enc, nil
}

func parseLevel(v string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("CW_LOG_LEVEL: unknown level %q", v)
	}
}
