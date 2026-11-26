package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bshadmehr98/candledb/internal/adapters/bcrypthash"
	"github.com/bshadmehr98/candledb/internal/adapters/httpapi"
	"github.com/bshadmehr98/candledb/internal/adapters/instrumented"
	"github.com/bshadmehr98/candledb/internal/adapters/jwtauth"
	"github.com/bshadmehr98/candledb/internal/adapters/sqlite"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/rules"
)

// config is everything "candledb serve" can be told. Every setting is a
// flag, and every flag falls back to an environment variable, which is the
// convenient form in containers.
type config struct {
	addr          string
	dataDir       string
	rulesFile     string
	devOpenRules  bool
	jwtSecret     string
	adminKey      string
	corsOrigins   string
	disableSignup bool
	trustProxy    bool
	tlsCert       string
	tlsKey        string
	logFormat     string
	logLevel      string
	accessTTL     time.Duration
	refreshTTL    time.Duration
	authRate      float64
	authBurst     int
}

func env(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

func parseServeFlags(args []string, stderr io.Writer) (config, error) {
	var c config
	flags := flag.NewFlagSet("candledb serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&c.addr, "addr", env("CANDLEDB_ADDR", ":8090"), "address to listen on [CANDLEDB_ADDR]")
	flags.StringVar(&c.dataDir, "data-dir", env("CANDLEDB_DATA_DIR", "./candledb-data"), "directory for the database and generated secrets [CANDLEDB_DATA_DIR]")
	flags.StringVar(&c.rulesFile, "rules", env("CANDLEDB_RULES", ""), "security rules file (default: <data-dir>/candledb.rules) [CANDLEDB_RULES]")
	flags.BoolVar(&c.devOpenRules, "dev-open-rules", env("CANDLEDB_DEV_OPEN_RULES", "") == "true", "allow every read and write; for local development only [CANDLEDB_DEV_OPEN_RULES]")
	flags.StringVar(&c.jwtSecret, "jwt-secret", env("CANDLEDB_JWT_SECRET", ""), "secret for signing access tokens, at least 32 bytes (default: generated and kept in <data-dir>) [CANDLEDB_JWT_SECRET]")
	flags.StringVar(&c.adminKey, "admin-key", env("CANDLEDB_ADMIN_KEY", ""), "bearer key that bypasses security rules; empty disables admin access [CANDLEDB_ADMIN_KEY]")
	flags.StringVar(&c.corsOrigins, "cors-origins", env("CANDLEDB_CORS_ORIGINS", "*"), "comma-separated browser origins allowed to call the API [CANDLEDB_CORS_ORIGINS]")
	flags.BoolVar(&c.disableSignup, "disable-signup", env("CANDLEDB_DISABLE_SIGNUP", "") == "true", "refuse new account registrations [CANDLEDB_DISABLE_SIGNUP]")
	flags.BoolVar(&c.trustProxy, "trust-proxy", env("CANDLEDB_TRUST_PROXY", "") == "true", "take client addresses from X-Forwarded-For [CANDLEDB_TRUST_PROXY]")
	flags.StringVar(&c.tlsCert, "tls-cert", env("CANDLEDB_TLS_CERT", ""), "TLS certificate file; serves HTTPS when set with -tls-key [CANDLEDB_TLS_CERT]")
	flags.StringVar(&c.tlsKey, "tls-key", env("CANDLEDB_TLS_KEY", ""), "TLS private key file [CANDLEDB_TLS_KEY]")
	flags.StringVar(&c.logFormat, "log-format", env("CANDLEDB_LOG_FORMAT", "text"), "log format: text or json [CANDLEDB_LOG_FORMAT]")
	flags.StringVar(&c.logLevel, "log-level", env("CANDLEDB_LOG_LEVEL", "info"), "log level: debug, info, warn or error [CANDLEDB_LOG_LEVEL]")
	// These four are not strings, so their environment fallback is applied
	// with Set, which parses text exactly as the command line would.
	flags.DurationVar(&c.accessTTL, "access-ttl", time.Hour, "lifetime of access tokens [CANDLEDB_ACCESS_TTL]")
	flags.DurationVar(&c.refreshTTL, "refresh-ttl", 30*24*time.Hour, "lifetime of refresh tokens [CANDLEDB_REFRESH_TTL]")
	flags.Float64Var(&c.authRate, "auth-rate", 30, "sign-up, sign-in and refresh attempts allowed per minute per client address [CANDLEDB_AUTH_RATE]")
	flags.IntVar(&c.authBurst, "auth-burst", 10, "such attempts allowed in a single burst [CANDLEDB_AUTH_BURST]")
	for name, variable := range map[string]string{
		"access-ttl": "CANDLEDB_ACCESS_TTL", "refresh-ttl": "CANDLEDB_REFRESH_TTL",
		"auth-rate": "CANDLEDB_AUTH_RATE", "auth-burst": "CANDLEDB_AUTH_BURST",
	} {
		if v, ok := os.LookupEnv(variable); ok {
			if err := flags.Set(name, v); err != nil {
				return c, fmt.Errorf("%s: %w", variable, err)
			}
		}
	}
	if err := flags.Parse(args); err != nil {
		return c, err
	}
	if (c.tlsCert == "") != (c.tlsKey == "") {
		return c, errors.New("-tls-cert and -tls-key must be given together")
	}
	if c.authRate <= 0 || c.authBurst <= 0 {
		return c, errors.New("-auth-rate and -auth-burst must be positive")
	}
	if c.adminKey != "" && len(c.adminKey) < 24 {
		return c, errors.New("-admin-key must be at least 24 characters")
	}
	return c, nil
}

func newLogger(c config, w io.Writer) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.logLevel)); err != nil {
		return nil, fmt.Errorf("unknown log level %q", c.logLevel)
	}
	opts := &slog.HandlerOptions{Level: level}
	switch c.logFormat {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("unknown log format %q", c.logFormat)
}

// serve runs the server until ctx is cancelled.
func serve(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := parseServeFlags(args, stderr)
	if err != nil {
		return err
	}
	logger, err := newLogger(cfg, stderr)
	if err != nil {
		return err
	}

	// 0700: the directory holds the database and the signing secret, so
	// only the server's own user may enter it.
	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	db, err := sqlite.Open(ctx, filepath.Join(cfg.dataDir, "candledb.db"))
	if err != nil {
		return err
	}
	// defer runs when serve returns, by any path. Deferred calls run in
	// reverse order, so the database closes after everything that uses it.
	defer db.Close()

	ruleset, source, err := loadRules(cfg)
	if err != nil {
		return err
	}
	switch source {
	case "":
		logger.Warn("no rules file found: every request without the admin key will be denied",
			"expected", defaultRulesPath(cfg))
	case devRules:
		logger.Warn("DEVELOPMENT MODE: security rules are disabled, anyone can read and write everything")
	default:
		logger.Info("security rules loaded", "file", source)
	}

	// Assemble the hexagon: driven adapters, then the core, then the
	// driving adapter.
	store := instrumented.Wrap(db)
	docs, err := app.New(ctx, store, app.WithLogger(logger), app.WithRules(ruleset))
	if err != nil {
		return err
	}
	defer docs.Close()

	secret, err := jwtSecret(cfg)
	if err != nil {
		return err
	}
	tokens, err := jwtauth.New(secret, "candledb", app.SystemClock{})
	if err != nil {
		return err
	}
	auth, err := app.NewAuthService(db.Users(), bcrypthash.New(0), tokens, app.SystemClock{}, app.AuthConfig{
		AccessTTL:     cfg.accessTTL,
		RefreshTTL:    cfg.refreshTTL,
		DisableSignup: cfg.disableSignup,
		AdminKey:      cfg.adminKey,
	})
	if err != nil {
		return err
	}

	api := httpapi.New(docs, auth,
		httpapi.WithLogger(logger),
		httpapi.WithCORSOrigins(splitList(cfg.corsOrigins)),
		httpapi.WithTrustProxy(cfg.trustProxy),
		httpapi.WithStoreStats(store),
		httpapi.WithAuthRateLimit(cfg.authRate, cfg.authBurst),
	)

	server := &http.Server{
		Handler: api.Handler(),
		// Timeouts protect against clients that open a connection and then
		// send or read very slowly, tying up resources. The listen stream
		// extends its own write deadline message by message.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// Listening separately from serving means a port that is already in
	// use is reported here as a plain error, and lets "-addr :0" pick a
	// free port whose number we can log.
	listener, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	logger.Info("candledb is listening", "addr", listener.Addr().String(), "version", version, "tls", cfg.tlsCert != "")

	// The server runs in its own goroutine; its final error comes back on
	// a channel. The buffer of 1 lets that goroutine finish even if nobody
	// is left to receive.
	serveErr := make(chan error, 1)
	go func() {
		if cfg.tlsCert != "" {
			serveErr <- server.ServeTLS(listener, cfg.tlsCert, cfg.tlsKey)
		} else {
			serveErr <- server.Serve(listener)
		}
	}()

	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)

	for {
		select {
		case err := <-serveErr:
			return err
		case <-reload:
			// SIGHUP reloads the rules without a restart. A file that does
			// not parse is rejected and the current rules stay in force.
			ruleset, source, err := loadRules(cfg)
			if err != nil {
				logger.Error("rules reload failed, keeping the current rules", "error", err)
				continue
			}
			docs.SetRules(ruleset)
			logger.Info("security rules reloaded", "file", source)
		case <-ctx.Done():
			logger.Info("shutting down")
			// Listen streams never finish on their own, so they are closed
			// first; Shutdown then waits for ordinary requests in flight.
			docs.Close()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown: %w", err)
			}
			return nil
		}
	}
}

const devRules = "(dev-open-rules)"

func defaultRulesPath(cfg config) string { return filepath.Join(cfg.dataDir, "candledb.rules") }

// loadRules returns the rules to enforce and a description of where they
// came from ("" when there is no rules file and everything is denied).
func loadRules(cfg config) (*rules.Ruleset, string, error) {
	if cfg.devOpenRules {
		return rules.AllowAll(), devRules, nil
	}
	path := cfg.rulesFile
	if path == "" {
		path = defaultRulesPath(cfg)
	}
	src, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && cfg.rulesFile == "" {
		return rules.DenyAll(), "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("reading rules: %w", err)
	}
	ruleset, err := rules.Parse(string(src))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return ruleset, path, nil
}

// jwtSecret returns the token signing secret: the configured one, or one
// generated on first start and kept in the data directory so that tokens
// stay valid across restarts.
func jwtSecret(cfg config) ([]byte, error) {
	if cfg.jwtSecret != "" {
		return []byte(cfg.jwtSecret), nil
	}
	path := filepath.Join(cfg.dataDir, "jwt.secret")
	secret, err := os.ReadFile(path)
	if err == nil {
		return []byte(strings.TrimSpace(string(secret))), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("reading jwt secret: %w", err)
	}
	generated := rand.Text() + rand.Text()
	if err := os.WriteFile(path, []byte(generated+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("storing jwt secret: %w", err)
	}
	return []byte(generated), nil
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
