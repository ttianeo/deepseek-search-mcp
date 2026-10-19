// Package httpserver provides the public HTTP surface of the MCP relay:
// bearer-token authentication, CORS, request logging, health checks and
// graceful error handling around the MCP streamable HTTP handler.
package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"deepseek-search-mcp/internal/auth"
)

const (
	// EndpointPath is the MCP streamable HTTP endpoint.
	EndpointPath = "/mcp"
	requestIDKey = "X-Request-Id"
)

// Options configures the HTTP handler.
type Options struct {
	// Logger receives request and error logs. A nil logger uses slog.Default.
	Logger *slog.Logger
	// MaxBodyBytes limits MCP request bodies. Zero disables the local limit.
	MaxBodyBytes int64
	// AllowedOrigins lists exact CORS origins, or contains "*" to allow all.
	AllowedOrigins []string
	// Version is reported on the landing page.
	Version string
}

// New builds the full HTTP handler around the MCP streamable HTTP handler.
func New(mcpHandler http.Handler, opts Options) http.Handler {
	if mcpHandler == nil {
		panic("httpserver: nil MCP handler")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	protectedMCP := withBearerAuth(withBodyLimit(mcpHandler, opts.MaxBodyBytes))
	mcpRoute := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EndpointPath && r.URL.Path != EndpointPath+"/" {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "not_found",
				"message": "unknown MCP endpoint",
			})
			return
		}
		protectedMCP.ServeHTTP(w, r)
	})

	mux := http.NewServeMux()
	mux.Handle(EndpointPath, mcpRoute)
	mux.Handle(EndpointPath+"/", mcpRoute)
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/readyz", healthHandler)
	mux.HandleFunc("/", landingHandler(opts.Version))

	var h http.Handler = mux
	h = withCORS(h, opts.AllowedOrigins)
	h = withSecurityHeaders(h)
	h = withRecovery(h, logger)
	h = withAccessLog(h, logger)
	h = withRequestID(h)
	return h
}

func withBearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := auth.FromHeader(r.Header)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="deepseek-search-mcp"`)
			w.Header().Set("Cache-Control", "no-store")
			message := `缺少 Authorization 头，请使用 "Authorization: Bearer <DeepSeek API Key>" 连接此 MCP 服务。`
			if errors.Is(err, auth.ErrMalformed) {
				message = `Authorization 头格式无效，期望格式为 "Bearer <DeepSeek API Key>"。`
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error":   "unauthorized",
				"message": message,
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithToken(r.Context(), token)))
	})
}

func withBodyLimit(next http.Handler, maxBytes int64) http.Handler {
	if maxBytes <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBytes {
			w.Header().Set("Connection", "close")
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error":   "payload_too_large",
				"message": fmt.Sprintf("请求体超过大小限制（%d 字节）", maxBytes),
			})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == EndpointPath || r.URL.Path == EndpointPath+"/" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func withCORS(next http.Handler, allowedOrigins []string) http.Handler {
	allowAll := false
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		origin = strings.TrimRight(strings.TrimSpace(origin), "/")
		if origin == "" {
			continue
		}
		if origin == "*" {
			allowAll = true
			continue
		}
		allowed[origin] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
		if origin != "" {
			_, explicitlyAllowed := allowed[origin]
			if allowAll || explicitlyAllowed {
				if allowAll {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Add("Vary", "Origin")
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Mcp-Session-Id, MCP-Protocol-Version, Last-Event-ID, X-Request-Id")
				w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, MCP-Protocol-Version, X-Request-Id")
				w.Header().Set("Access-Control-Max-Age", "600")
			} else if r.Method == http.MethodOptions {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error":   "origin_not_allowed",
					"message": "CORS origin is not allowed",
				})
				return
			}
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type requestIDContextKey struct{}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDKey))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, id)))
	})
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

func sanitizeRequestID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 {
		return ""
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
		default:
			return ""
		}
	}
	return id
}

var fallbackRequestID atomic.Uint64

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), fallbackRequestID.Add(1))
}

func withAccessLog(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		if status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}

		logger.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", requestIDFrom(r.Context()),
			"remote_addr", r.RemoteAddr,
			"user_agent", truncate(r.UserAgent(), 256),
		)
	})
}

func withRecovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				logger.Error("panic while handling request",
					"panic", fmt.Sprint(v),
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", requestIDFrom(r.Context()),
					"stack", string(debug.Stack()),
				)
				if tw.wrote {
					w.Header().Set("Connection", "close")
					return
				}
				writeJSON(tw, http.StatusInternalServerError, map[string]string{
					"error":   "internal_error",
					"message": "internal server error",
				})
			}
		}()
		next.ServeHTTP(tw, r)
	})
}

type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *trackingWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(p)
}

func (w *trackingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *trackingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if r.status != 0 {
		return
	}
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error":   "method_not_allowed",
			"message": "use GET",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func landingHandler(version string) http.HandlerFunc {
	if version == "" {
		version = "dev"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "not_found",
				"message": "not found",
			})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
				"error":   "method_not_allowed",
				"message": "use GET",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":          "deepseek-search-mcp",
			"version":       version,
			"description":   "DeepSeek 搜索与网页抓取的公共 MCP 中转服务",
			"mcp_endpoint":  EndpointPath,
			"transport":     "streamable-http",
			"auth":          "Authorization: Bearer <DeepSeek API Key>",
			"tools":         []string{"search", "fetch"},
			"documentation": "https://api-docs.deepseek.com/zh-cn/guides/search",
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"internal_error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
