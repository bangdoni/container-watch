// Command containerwatch runs the ContainerWatch monitoring server.
//
// It performs zero-configuration startup: local Docker/Podman sockets are
// discovered automatically, the SQLite database is created on first run, and
// the first visit to the UI opens the setup wizard to create an admin account.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/containerwatch/containerwatch/internal/auth"
	"github.com/containerwatch/containerwatch/internal/config"
	"github.com/containerwatch/containerwatch/internal/database"
	"github.com/containerwatch/containerwatch/internal/engine"
	"github.com/containerwatch/containerwatch/internal/handlers"
	"github.com/containerwatch/containerwatch/internal/sse"
	"github.com/containerwatch/containerwatch/internal/version"
)

const (
	// shutdownGrace is how long in-flight requests get to finish on SIGTERM.
	shutdownGrace = 15 * time.Second
	// sessionPurgeEvery bounds how often expired sessions are swept.
	sessionPurgeEvery = time.Hour
	// nodeHealthEvery is the node re-probe cadence.
	nodeHealthEvery = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "containerwatch:", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print the version and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("containerwatch %s (commit %s, built %s, %s/%s, %s)\n",
			version.Version, version.Commit, version.BuildDate,
			runtime.GOOS, runtime.GOARCH, runtime.Version())
		return nil
	}
	if *healthcheck {
		// Container health probes run without shell or curl in the runtime
		// image, so the binary probes itself.
		return probeSelf()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := newLogger(cfg.LogLevel)
	log.Info("starting containerwatch",
		"version", version.String(),
		"addr", cfg.Addr,
		"database", cfg.DatabasePath,
		"metrics_interval", cfg.MetricsInterval.String(),
		"go", runtime.Version(),
	)

	// Root context cancelled on SIGINT/SIGTERM. Every background goroutine
	// (samplers, event watchers, health loop) derives from it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	authMgr := auth.NewManager(db, cfg.SessionTTL, cfg.SecureCookies, cfg.TrustProxy)

	hub := sse.NewHub()
	metrics := sse.NewMetricsBroker(hub, log, cfg.MetricsInterval, cfg.MetricsWindow)
	events := sse.NewEventBroker(hub, log)

	nodes := engine.NewManager(db, log, cfg.LocalSockets, cfg.DisableLocalDiscovery)
	bootCtx, bootCancel := context.WithTimeout(ctx, 20*time.Second)
	err = nodes.Bootstrap(bootCtx)
	bootCancel()
	if err != nil {
		// A node failure must not prevent the UI from starting: the operator
		// needs it to fix the node configuration.
		log.Warn("node discovery incomplete", "error", err)
	}
	go nodes.StartHealthLoop(ctx, nodeHealthEvery)

	srv, err := handlers.New(cfg, db, authMgr, nodes, hub, metrics, events, log)
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}
	for _, n := range nodes.List() {
		srv.StartNodeStreams(ctx, n)
		log.Info("node ready",
			"name", n.Record.Name,
			"endpoint", n.Record.Endpoint,
			"online", n.Status.Online,
			"version", n.Status.Version,
		)
	}

	go purgeSessionsLoop(ctx, db, log)

	// A signal received during startup must not be lost: check before binding.
	if ctx.Err() != nil {
		log.Info("shutdown requested during startup")
		return nil
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE responses are intentionally long-lived.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Info("shutdown requested")
	}

	// Stop accepting work before tearing down the background goroutines so
	// in-flight SSE connections drain naturally.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out", "error", err)
		_ = httpSrv.Close()
	}
	log.Info("stopped")
	return nil
}

// probeSelf requests /healthz on the configured listen address. It honours
// CW_HEALTHCHECK_URL for deployments where the server binds a different
// interface than the probe should use.
func probeSelf() error {
	target := os.Getenv("CW_HEALTHCHECK_URL")
	if target == "" {
		addr := os.Getenv("CW_ADDR")
		if addr == "" {
			addr = ":8080"
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("healthcheck: parse CW_ADDR: %w", err)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		target = "http://" + net.JoinHostPort(host, port) + "/healthz"
	}

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s returned %s", target, resp.Status)
	}
	fmt.Println("ok")
	return nil
}

// newLogger builds a text logger on stderr with source locations disabled to
// keep container logs readable.
func newLogger(level slog.Level) *slog.Logger {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}

// purgeSessionsLoop removes expired sessions periodically, and once shortly
// after boot so a restart cleans up after itself.
func purgeSessionsLoop(ctx context.Context, db *database.Store, log *slog.Logger) {
	purge := func() {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		n, err := db.PurgeExpiredSessions(pctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("purge sessions", "error", err)
			}
			return
		}
		if n > 0 {
			log.Info("purged expired sessions", "count", n)
		}
	}
	purge()
	t := time.NewTicker(sessionPurgeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}
