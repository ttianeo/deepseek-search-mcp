package httpserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"deepseek-search-mcp/internal/auth"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type tokenRecorder struct {
	mu    sync.Mutex
	token string
}

func (r *tokenRecorder) set(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.token = token
}

func (r *tokenRecorder) get() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.token
}

func newTestServer(t *testing.T, handler http.Handler, opts Options) *httptest.Server {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = testLogger()
	}
	ts := httptest.NewServer(New(handler, opts))
	t.Cleanup(ts.Close)
	return ts
}

func doRequest(t *testing.T, ts *httptest.Server, method, path string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestBearerAuth(t *testing.T) {
	rec := &tokenRecorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := auth.FromContext(r.Context())
		rec.set(token)
		w.WriteHeader(http.StatusOK)
	})
	ts := newTestServer(t, handler, Options{MaxBodyBytes: 1 << 20})

	t.Run("missing", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodPost, EndpointPath, strings.NewReader(`{}`), nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("WWW-Authenticate = %q", resp.Header.Get("WWW-Authenticate"))
		}
	})
	t.Run("malformed", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodPost, EndpointPath, strings.NewReader(`{}`), map[string]string{
			"Authorization": "Basic abc",
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
	t.Run("valid", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodPost, EndpointPath, strings.NewReader(`{}`), map[string]string{
			"Authorization": "Bearer sk-caller-token",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := rec.get(); got != "sk-caller-token" {
			t.Errorf("handler token = %q, want sk-caller-token", got)
		}
	})
	t.Run("unknown mcp path with auth", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodPost, EndpointPath+"/other", strings.NewReader(`{}`), map[string]string{
			"Authorization": "Bearer sk-caller-token",
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}

func TestCORS(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("allow all preflight bypasses auth", func(t *testing.T) {
		ts := newTestServer(t, handler, Options{AllowedOrigins: []string{"*"}})
		resp := doRequest(t, ts, http.MethodOptions, EndpointPath, nil, map[string]string{
			"Origin":                        "https://example.com",
			"Access-Control-Request-Method": "POST",
		})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Allow-Origin = %q, want *", got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
			t.Errorf("Allow-Headers = %q, want Authorization", got)
		}
	})

	t.Run("explicit origin", func(t *testing.T) {
		ts := newTestServer(t, handler, Options{AllowedOrigins: []string{"https://app.example.com"}})
		resp := doRequest(t, ts, http.MethodOptions, EndpointPath, nil, map[string]string{
			"Origin": "https://app.example.com",
		})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
			t.Errorf("Allow-Origin = %q", got)
		}
		if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("Vary = %q, want Origin", got)
		}
	})

	t.Run("disallowed origin", func(t *testing.T) {
		ts := newTestServer(t, handler, Options{AllowedOrigins: []string{"https://app.example.com"}})
		resp := doRequest(t, ts, http.MethodOptions, EndpointPath, nil, map[string]string{
			"Origin": "https://evil.example.com",
		})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin = %q, want empty", got)
		}
	})
}

func TestHealthAndLanding(t *testing.T) {
	ts := newTestServer(t, http.NotFoundHandler(), Options{Version: "1.2.3"})

	t.Run("health", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodGet, "/healthz", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["status"] != "ok" {
			t.Errorf("body = %v", body)
		}
	})

	t.Run("health method not allowed", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodPost, "/healthz", nil, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("landing", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodGet, "/", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["mcp_endpoint"] != EndpointPath || body["version"] != "1.2.3" {
			t.Errorf("body = %v", body)
		}
	})

	t.Run("unknown path", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodGet, "/nope", nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}

func TestBodyLimit(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	ts := newTestServer(t, handler, Options{MaxBodyBytes: 16})

	resp := doRequest(t, ts, http.MethodPost, EndpointPath, strings.NewReader(strings.Repeat("x", 100)), map[string]string{
		"Authorization": "Bearer sk-test",
	})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestRequestID(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := newTestServer(t, handler, Options{})

	t.Run("echo valid incoming id", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodGet, "/healthz", nil, map[string]string{
			"X-Request-Id": "abc-123",
		})
		if got := resp.Header.Get("X-Request-Id"); got != "abc-123" {
			t.Errorf("X-Request-Id = %q, want abc-123", got)
		}
	})

	t.Run("replace invalid incoming id", func(t *testing.T) {
		resp := doRequest(t, ts, http.MethodGet, "/healthz", nil, map[string]string{
			"X-Request-Id": "bad value",
		})
		got := resp.Header.Get("X-Request-Id")
		if got == "" || got == "bad value" || len(got) != 16 {
			t.Errorf("X-Request-Id = %q, want generated 16-char id", got)
		}
	})
}

func TestRecovery(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	ts := newTestServer(t, handler, Options{})

	resp := doRequest(t, ts, http.MethodPost, EndpointPath, strings.NewReader(`{}`), map[string]string{
		"Authorization": "Bearer sk-test",
	})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "internal_error") {
		t.Errorf("body = %q", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t, http.NotFoundHandler(), Options{})
	resp := doRequest(t, ts, http.MethodGet, "/healthz", nil, nil)
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
}
