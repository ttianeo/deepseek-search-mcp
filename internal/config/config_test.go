package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
	if c.Addr != ":8080" || !c.Stateless || !c.JSONResponse {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.RequestTimeout != 60*time.Second || c.ShutdownTimeout != 10*time.Second {
		t.Errorf("unexpected timeouts: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("ADDR", "127.0.0.1:9090")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("DEEPSEEK_BASE_URL", "https://example.com/api")
	t.Setenv("DEEPSEEK_TIMEOUT", "12s")
	t.Setenv("MCP_MAX_BODY_BYTES", "2048")
	t.Setenv("MAX_UPSTREAM_RESPONSE_BYTES", "4096")
	t.Setenv("MCP_STATELESS", "false")
	t.Setenv("MCP_JSON_RESPONSE", "false")
	t.Setenv("MCP_SESSION_TIMEOUT", "5m")
	t.Setenv("SHUTDOWN_TIMEOUT", "3s")
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://a.example, https://b.example,")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr != "127.0.0.1:9090" {
		t.Errorf("Addr = %q", c.Addr)
	}
	if c.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", c.LogLevel)
	}
	if c.DeepSeekBaseURL != "https://example.com/api" {
		t.Errorf("DeepSeekBaseURL = %q", c.DeepSeekBaseURL)
	}
	if c.RequestTimeout != 12*time.Second || c.SessionTimeout != 5*time.Minute || c.ShutdownTimeout != 3*time.Second {
		t.Errorf("timeouts = %+v", c)
	}
	if c.MaxBodyBytes != 2048 || c.MaxUpstreamResponseBytes != 4096 {
		t.Errorf("limits = %d, %d", c.MaxBodyBytes, c.MaxUpstreamResponseBytes)
	}
	if c.Stateless || c.JSONResponse {
		t.Errorf("booleans = %v, %v", c.Stateless, c.JSONResponse)
	}
	if len(c.CORSAllowedOrigins) != 2 || c.CORSAllowedOrigins[0] != "https://a.example" || c.CORSAllowedOrigins[1] != "https://b.example" {
		t.Errorf("CORSAllowedOrigins = %v", c.CORSAllowedOrigins)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{"LOG_LEVEL", "verbose"},
		{"DEEPSEEK_TIMEOUT", "not-a-duration"},
		{"DEEPSEEK_TIMEOUT", "0s"},
		{"MCP_MAX_BODY_BYTES", "-1"},
		{"MAX_UPSTREAM_RESPONSE_BYTES", "abc"},
		{"MCP_STATELESS", "maybe"},
		{"MCP_JSON_RESPONSE", "maybe"},
		{"MCP_SESSION_TIMEOUT", "-1m"},
		{"SHUTDOWN_TIMEOUT", "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() with %s=%q = nil error, want error", tt.key, tt.value)
			}
		})
	}
}

func TestValidateRejectsEmptyValues(t *testing.T) {
	base := Default()
	tests := map[string]func(*Config){
		"addr":       func(c *Config) { c.Addr = "" },
		"base url":   func(c *Config) { c.DeepSeekBaseURL = "" },
		"timeout":    func(c *Config) { c.RequestTimeout = 0 },
		"body limit": func(c *Config) { c.MaxBodyBytes = 0 },
		"resp limit": func(c *Config) { c.MaxUpstreamResponseBytes = 0 },
		"session":    func(c *Config) { c.SessionTimeout = 0 },
		"shutdown":   func(c *Config) { c.ShutdownTimeout = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			c := base
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}
