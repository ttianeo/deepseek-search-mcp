// Command deepseek-search-mcp is a public, stateless MCP relay for the
// DeepSeek Search and Fetch APIs.
//
// Every caller supplies their own DeepSeek API key in the HTTP Authorization
// header; the relay forwards it upstream and never stores it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"deepseek-search-mcp/internal/config"
	"deepseek-search-mcp/internal/deepseek"
	"deepseek-search-mcp/internal/httpserver"
	"deepseek-search-mcp/internal/mcpserver"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "deepseek-search-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	handler, err := newApplication(cfg, logger)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout is intentionally zero: tool calls can take up to
		// DEEPSEEK_TIMEOUT, and SSE responses must remain open.
		WriteTimeout:   0,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 << 10,
		ErrorLog:       slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	logger.Info("starting deepseek-search-mcp",
		"version", version,
		"addr", cfg.Addr,
		"deepseek_base_url", cfg.DeepSeekBaseURL,
		"stateless", cfg.Stateless,
		"json_response", cfg.JSONResponse,
		"request_timeout", cfg.RequestTimeout.String(),
		"max_body_bytes", cfg.MaxBodyBytes,
		"max_upstream_response_bytes", cfg.MaxUpstreamResponseBytes,
	)
	warnInsecureBaseURL(logger, cfg.DeepSeekBaseURL)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			return fmt.Errorf("HTTP server: %w", err)
		}
		return nil
	case <-sigCtx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("server stopped")
	return nil
}

// newApplication wires the DeepSeek client, MCP server and HTTP middleware.
// It is separate from run so tests can exercise the complete request path.
func newApplication(cfg config.Config, logger *slog.Logger) (http.Handler, error) {
	client, err := deepseek.New(
		cfg.DeepSeekBaseURL,
		deepseek.WithTimeout(cfg.RequestTimeout),
		deepseek.WithMaxResponseBytes(cfg.MaxUpstreamResponseBytes),
		deepseek.WithUserAgent("deepseek-search-mcp/"+version),
	)
	if err != nil {
		return nil, fmt.Errorf("creating DeepSeek client: %w", err)
	}

	sdkLog := sdkLogger(logger)
	mcpServer, err := mcpserver.New(client, version, sdkLog)
	if err != nil {
		return nil, fmt.Errorf("creating MCP server: %w", err)
	}

	streamHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{
			Stateless:      cfg.Stateless,
			JSONResponse:   cfg.JSONResponse,
			Logger:         sdkLog,
			SessionTimeout: cfg.SessionTimeout,
			// The public deployment normally sits behind a same-host reverse
			// proxy, which makes every request appear to originate from
			// localhost. DNS-rebinding protection would reject those requests,
			// and it is not needed here because every call requires the
			// caller's own bearer token.
			DisableLocalhostProtection: true,
			MaxRequestBodyBytes:        cfg.MaxBodyBytes,
		},
	)

	return httpserver.New(streamHandler, httpserver.Options{
		Logger:         logger,
		MaxBodyBytes:   cfg.MaxBodyBytes,
		AllowedOrigins: cfg.CORSAllowedOrigins,
		Version:        version,
	}), nil
}

// sdkLogger suppresses the MCP SDK's per-request Info messages; warnings and
// errors still reach the application logger.
func sdkLogger(logger *slog.Logger) *slog.Logger {
	return slog.New(&minLevelHandler{next: logger.Handler(), min: slog.LevelWarn})
}

type minLevelHandler struct {
	next slog.Handler
	min  slog.Level
}

func (h *minLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.min && h.next.Enabled(ctx, level)
}

func (h *minLevelHandler) Handle(ctx context.Context, record slog.Record) error {
	return h.next.Handle(ctx, record)
}

func (h *minLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &minLevelHandler{next: h.next.WithAttrs(attrs), min: h.min}
}

func (h *minLevelHandler) WithGroup(name string) slog.Handler {
	return &minLevelHandler{next: h.next.WithGroup(name), min: h.min}
}

func warnInsecureBaseURL(logger *slog.Logger, rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return
	}
	host := u.Hostname()
	if host == "localhost" {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	logger.Warn("DEEPSEEK_BASE_URL uses plain HTTP; the caller's API key will be sent without TLS", "host", host)
}
