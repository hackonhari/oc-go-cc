// Package server manages the HTTP server lifecycle.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"oc-go-cc/internal/client"
	"oc-go-cc/internal/config"
	"oc-go-cc/internal/freepool"
	"oc-go-cc/internal/handlers"
	"oc-go-cc/internal/keypool"
	"oc-go-cc/internal/metrics"
	"oc-go-cc/internal/router"
	"oc-go-cc/internal/token"
)

// stateFilePath is where the keypool persists runtime key state.
const stateFilePath = "~/.config/oc-go-cc/key-states.json"

// resolveFreeFallback picks the effective free_fallback config: state
// file wins when it has any models, otherwise fall back to config.json
// defaults. This lets operators add/remove free models via the same
// hot-reloadable key-states.json that holds the paid key pool.
//
// Before 2026-05-18 cycle 2 fix, state file's free_fallback was
// decorative-only — the freepool always read config.json values,
// causing silent drift between Go defaults and runtime.
//
// Returns base URL, models, and a short source label for logging
// ("state-file" | "config-default").
func resolveFreeFallback(cfgFB config.FreeFallback, stateFB keypool.FreeFallback) (string, []string, string) {
	if len(stateFB.Models) > 0 {
		baseURL := stateFB.BaseURL
		if baseURL == "" {
			baseURL = cfgFB.BaseURL // state file specified models but no URL — borrow config URL
		}
		return baseURL, stateFB.Models, "state-file"
	}
	return cfgFB.BaseURL, cfgFB.Models, "config-default"
}

// rotationLogDir is where rotation events land as daily JSONL files.
const rotationLogDir = "~/.cache/oc-go-cc"

// Server represents the proxy server.
type Server struct {
	config  *config.Config
	httpSrv *http.Server
	logger  *slog.Logger
}

// NewServer creates a new proxy server.
func NewServer(cfg *config.Config) (*Server, error) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.Logging.Level),
	}))
	slog.SetDefault(logger)

	// Initialize components.
	tokenCounter, err := token.NewCounter()
	if err != nil {
		return nil, fmt.Errorf("failed to create token counter: %w", err)
	}

	// Create metrics
	metrics := metrics.New()

	// Initialize keypool: load runtime state from disk; seed from config
	// if state file absent (first run after Phase 1 deploy).
	pool, err := initKeyPool(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("init keypool: %w", err)
	}

	// Start hot-reload watcher so external edits to key-states.json
	// (e.g. adding a 3rd account) are picked up without proxy restart.
	pool.StartHotReload(context.Background(), keypool.DefaultHotReloadInterval)

	// Rotation event log — daily JSONL at ~/.cache/oc-go-cc/rotation-YYYYMMDD.log.
	// Wired into pool (key acquired/exhausted/transient/reset/revived events),
	// revalidator (started/tick events), AND freepool (fallback engagement).
	// Constructed BEFORE revalidator so we can pass it in. Pool gets it next.
	eventLogger := keypool.NewEventLogger(expandHomePath(rotationLogDir), logger)
	pool.SetEmitter(eventLogger)

	// Start revalidator — every 6h, probe each exhausted key with a tiny
	// request. HTTP 200 → clear exhaustion (auto-heals classifier false
	// positives). Emits revalidator_started/tick events to rotation log so
	// operators can verify health from the same channel as paid-pool events.
	revalidator := keypool.NewRevalidator(pool, cfg.OpenCodeGo.BaseURL, logger, eventLogger)
	go revalidator.Run(context.Background())

	classifier := keypool.NewClassifier()

	freeBaseURL, freeModels, source := resolveFreeFallback(cfg.FreeFallback, pool.FreeFallback())
	logger.Info("free_fallback resolved",
		"source", source, "base_url", freeBaseURL, "models_count", len(freeModels))
	freeResolver := freepool.New(freeBaseURL, freeModels, eventLogger)
	openCodeClient := client.NewOpenCodeClient(cfg.OpenCodeGo, pool, classifier, freeResolver)
	modelRouter := router.NewModelRouter(cfg)
	fallbackHandler := router.NewFallbackHandler(logger, 3, 30*time.Second)

	// Create handlers.
	messagesHandler := handlers.NewMessagesHandler(
		cfg,
		openCodeClient,
		modelRouter,
		fallbackHandler,
		tokenCounter,
		metrics,
	)
	healthHandler := handlers.NewHealthHandler(tokenCounter, fallbackHandler, metrics)

	// Setup router.
	mux := http.NewServeMux()

	// API routes — normal flow (paid pool → free fallback on exhaustion).
	mux.HandleFunc("/v1/messages", messagesHandler.HandleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", healthHandler.HandleCountTokens)
	mux.HandleFunc("/health", healthHandler.HandleHealth)

	// Free-only routes — skip paid pool entirely; serve from Zen free
	// models anonymously. Used by `hh-cd -pr ocgo --free` to preserve
	// paid quota for sessions that don't need premium model quality.
	mux.HandleFunc("/free/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		ctx := client.WithFreeOnly(r.Context())
		messagesHandler.HandleMessages(w, r.WithContext(ctx))
	})
	mux.HandleFunc("/free/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		// Token counting is endpoint-independent; reuse normal handler.
		healthHandler.HandleCountTokens(w, r)
	})

	// Create HTTP server.
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // disabled — streaming SSE may run hours/days
		IdleTimeout:  120 * time.Second,
	}

	return &Server{
		config:  cfg,
		httpSrv: httpSrv,
		logger:  logger,
	}, nil
}

// Start starts the server with graceful shutdown.
func (s *Server) Start() error {
	s.logger.Info("starting oc-go-cc proxy",
		"host", s.config.Host,
		"port", s.config.Port,
		"base_url", s.config.OpenCodeGo.BaseURL,
	)

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		s.logger.Info("shutting down server...")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("server shutdown failed", "error", err)
		}
	}()

	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server failed: %w", err)
	}

	s.logger.Info("server stopped")
	return nil
}

// WritePID writes the current PID to a file.
func WritePID(path string) error {
	pid := os.Getpid()
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", pid)), 0644)
}

// ReadPID reads the PID from a file.
func ReadPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	var pid int
	_, err = fmt.Sscanf(string(data), "%d", &pid)
	return pid, err
}

// initKeyPool constructs the keypool, loads its state from disk if
// present, and seeds from config.APIKeys when the state file is absent.
//
// Reconciliation: if state file exists but the config has keys not yet
// present in state, those new keys are appended as ACTIVE entries. This
// makes adding a key via config.json (rather than direct state-file edit)
// also work smoothly.
func initKeyPool(cfg *config.Config, logger *slog.Logger) (*keypool.KeyPool, error) {
	path := expandHomePath(stateFilePath)
	pool := keypool.New(path, logger)

	if err := pool.LoadFromDisk(); err != nil {
		return nil, fmt.Errorf("load state file: %w", err)
	}

	// If state is empty AND we have config keys, seed.
	if len(pool.Snapshot()) == 0 && len(cfg.APIKeys) > 0 {
		seedKeys := make([]*keypool.Key, 0, len(cfg.APIKeys))
		for _, k := range cfg.APIKeys {
			seedKeys = append(seedKeys, &keypool.Key{
				Token:     k.Token,
				Account:   k.Account,
				ResetDate: defaultSeedResetDate(),
			})
		}
		fb := keypool.FreeFallback{
			BaseURL: cfg.FreeFallback.BaseURL,
			Models:  cfg.FreeFallback.Models,
		}
		if err := pool.SeedKeys(seedKeys, fb); err != nil {
			return nil, fmt.Errorf("seed keypool: %w", err)
		}
		logger.Info("keypool seeded from config", "keys", len(seedKeys))
	} else {
		logger.Info("keypool loaded from state file", "keys", len(pool.Snapshot()))
	}

	return pool, nil
}

// defaultSeedResetDate is the placeholder reset date for newly-seeded
// keys. 30 days out matches OpenCode's monthly cap cycle.
func defaultSeedResetDate() time.Time {
	return time.Now().Add(30 * 24 * time.Hour)
}

// expandHomePath replaces a leading "~/" with the user's home directory.
func expandHomePath(path string) string {
	if len(path) >= 2 && path[0] == '~' && path[1] == '/' {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home + path[1:]
	}
	return path
}

// parseLogLevel converts a string log level to slog.Level.
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
