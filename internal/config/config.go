// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"deepseek-search-mcp/internal/deepseek"
)

// Config is the runtime configuration of the MCP relay.
type Config struct {
	Addr                     string
	LogLevel                 slog.Level
	DeepSeekBaseURL          string
	RequestTimeout           time.Duration
	MaxBodyBytes             int64
	MaxUpstreamResponseBytes int64
	Stateless                bool
	JSONResponse             bool
	SessionTimeout           time.Duration
	ShutdownTimeout          time.Duration
	CORSAllowedOrigins       []string
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Addr:                     ":8080",
		LogLevel:                 slog.LevelInfo,
		DeepSeekBaseURL:          deepseek.DefaultBaseURL,
		RequestTimeout:           deepseek.DefaultTimeout,
		MaxBodyBytes:             1 << 20,
		MaxUpstreamResponseBytes: deepseek.DefaultMaxResponseBytes,
		Stateless:                true,
		JSONResponse:             true,
		SessionTimeout:           30 * time.Minute,
		ShutdownTimeout:          10 * time.Second,
		CORSAllowedOrigins:       []string{"*"},
	}
}

// Load reads configuration from the environment, applying Default for unset
// variables.
func Load() (Config, error) {
	c := Default()
	var err error

	c.Addr = envString("ADDR", c.Addr)
	c.DeepSeekBaseURL = envString("DEEPSEEK_BASE_URL", c.DeepSeekBaseURL)

	if v := envString("LOG_LEVEL", ""); v != "" {
		c.LogLevel, err = parseLogLevel(v)
		if err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	if c.RequestTimeout, err = envDuration("DEEPSEEK_TIMEOUT", c.RequestTimeout); err != nil {
		return Config{}, err
	}
	if c.SessionTimeout, err = envDuration("MCP_SESSION_TIMEOUT", c.SessionTimeout); err != nil {
		return Config{}, err
	}
	if c.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if c.MaxBodyBytes, err = envInt64("MCP_MAX_BODY_BYTES", c.MaxBodyBytes); err != nil {
		return Config{}, err
	}
	if c.MaxUpstreamResponseBytes, err = envInt64("MAX_UPSTREAM_RESPONSE_BYTES", c.MaxUpstreamResponseBytes); err != nil {
		return Config{}, err
	}
	if c.Stateless, err = envBool("MCP_STATELESS", c.Stateless); err != nil {
		return Config{}, err
	}
	if c.JSONResponse, err = envBool("MCP_JSON_RESPONSE", c.JSONResponse); err != nil {
		return Config{}, err
	}
	if origins, ok := os.LookupEnv("CORS_ALLOWED_ORIGINS"); ok {
		c.CORSAllowedOrigins = splitList(origins)
	}

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks that all values are usable.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return fmt.Errorf("ADDR must not be empty")
	}
	if strings.TrimSpace(c.DeepSeekBaseURL) == "" {
		return fmt.Errorf("DEEPSEEK_BASE_URL must not be empty")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("DEEPSEEK_TIMEOUT must be positive")
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("MCP_MAX_BODY_BYTES must be positive")
	}
	if c.MaxUpstreamResponseBytes <= 0 {
		return fmt.Errorf("MAX_UPSTREAM_RESPONSE_BYTES must be positive")
	}
	if c.SessionTimeout <= 0 {
		return fmt.Errorf("MCP_SESSION_TIMEOUT must be positive")
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("SHUTDOWN_TIMEOUT must be positive")
	}
	return nil
}

func envString(key, def string) string {
	if value, ok := os.LookupEnv(key); ok {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, d)
	}
	return d, nil
}

func envInt64(key string, def int64) (int64, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %d", key, n)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown level %q (want debug, info, warn or error)", value)
	}
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
