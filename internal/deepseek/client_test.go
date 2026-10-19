package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type capturedRequest struct {
	method      string
	path        string
	auth        string
	contentType string
	userAgent   string
	body        []byte
}

type capture struct {
	mu  sync.Mutex
	req capturedRequest
}

func (c *capture) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.req = capturedRequest{
		method:      r.Method,
		path:        r.URL.Path,
		auth:        r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"),
		userAgent:   r.Header.Get("User-Agent"),
		body:        body,
	}
}

func (c *capture) snapshot() capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	req := c.req
	req.body = append([]byte(nil), c.req.body...)
	return req
}

func newTestClient(t *testing.T, baseURL string, opts ...Option) *Client {
	t.Helper()
	c, err := New(baseURL, opts...)
	if err != nil {
		t.Fatalf("New(%q): %v", baseURL, err)
	}
	return c
}

func TestSearchSuccess(t *testing.T) {
	var got capture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"results": [{
				"rerank_score": 2.4,
				"title": "DeepSeek V4 模型发布",
				"url": "https://example.com/deepseek-v4",
				"snippet": "短摘要",
				"summary": "长摘要",
				"content": "网页正文",
				"site_name": "Example News",
				"site_icon": "https://example.com/favicon.ico",
				"date": "2026-06-01T00:00:00+00:00",
				"authority": 3
			}]
		}`)
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL, WithUserAgent("test-agent/1.0"))
	resp, err := c.Search(context.Background(), "sk-test-token", SearchRequest{
		Query:       "DeepSeek V4",
		Count:       5,
		OriQuestion: "DeepSeek 什么时候发布 V4？",
		NeedContent: true,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	snap := got.snapshot()
	if snap.method != http.MethodPost {
		t.Errorf("method = %q, want POST", snap.method)
	}
	if snap.path != SearchPath {
		t.Errorf("path = %q, want %q", snap.path, SearchPath)
	}
	if snap.auth != "Bearer sk-test-token" {
		t.Errorf("Authorization = %q", snap.auth)
	}
	if snap.contentType != "application/json" {
		t.Errorf("Content-Type = %q", snap.contentType)
	}
	if snap.userAgent != "test-agent/1.0" {
		t.Errorf("User-Agent = %q", snap.userAgent)
	}

	var sent SearchRequest
	if err := json.Unmarshal(snap.body, &sent); err != nil {
		t.Fatalf("decoding request body: %v", err)
	}
	if sent.Query != "DeepSeek V4" || sent.Count != 5 || !sent.NeedContent || sent.OriQuestion == "" {
		t.Errorf("unexpected request body: %+v", sent)
	}

	if len(resp.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(resp.Results))
	}
	r := resp.Results[0]
	if r.RerankScore != 2.4 || r.Title != "DeepSeek V4 模型发布" || r.URL == "" || r.Authority != 3 {
		t.Errorf("unexpected result: %+v", r)
	}
	if r.SiteName == nil || *r.SiteName != "Example News" {
		t.Errorf("SiteName = %v", r.SiteName)
	}
	if r.Date == nil || *r.Date != "2026-06-01T00:00:00+00:00" {
		t.Errorf("Date = %v", r.Date)
	}
	if r.Content != "网页正文" {
		t.Errorf("Content = %q", r.Content)
	}
}

func TestSearchOmitsDefaults(t *testing.T) {
	var got capture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		io.WriteString(w, `{"results":[]}`)
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL)
	resp, err := c.Search(context.Background(), "sk-test", SearchRequest{Query: "golang"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Results == nil {
		t.Fatal("Results should be a non-nil empty slice")
	}

	snap := got.snapshot()
	var body map[string]any
	if err := json.Unmarshal(snap.body, &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	for _, key := range []string{"count", "need_content", "ori_question", "localization"} {
		if _, ok := body[key]; ok {
			t.Errorf("request body unexpectedly contains %q: %s", key, snap.body)
		}
	}
	if body["query"] != "golang" {
		t.Errorf("query = %v", body["query"])
	}
}

func TestFetchSuccess(t *testing.T) {
	var got capture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		io.WriteString(w, `{"title":"Example article","content":"# Example article\n\nArticle content."}`)
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL)
	resp, err := c.Fetch(context.Background(), "sk-test", "https://example.com/article")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	snap := got.snapshot()
	if snap.path != FetchPath {
		t.Errorf("path = %q, want %q", snap.path, FetchPath)
	}
	if snap.auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", snap.auth)
	}
	var body map[string]string
	if err := json.Unmarshal(snap.body, &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if body["url"] != "https://example.com/article" {
		t.Errorf("url = %q", body["url"])
	}
	if resp.Title != "Example article" || !strings.Contains(resp.Content, "Article content") {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestValidateFetchURL(t *testing.T) {
	valid := []string{
		"https://example.com",
		"http://example.com/path?q=1#frag",
		"HTTPS://EXAMPLE.COM/path",
		"https://example.com:8443/a%20b",
		"https://[2001:db8::1]/x",
	}
	for _, rawURL := range valid {
		if err := ValidateFetchURL(rawURL); err != nil {
			t.Errorf("ValidateFetchURL(%q) = %v, want nil", rawURL, err)
		}
	}

	invalid := []string{
		"",
		"   ",
		"ftp://example.com",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"https://",
		"https:///path",
		"http://:8080",
		"//example.com",
		"example.com",
		"https://user:pass@example.com",
		"https://example.com/a b",
		"https://example.com/\n",
		strings.Repeat("a", 8193),
	}
	for _, rawURL := range invalid {
		if err := ValidateFetchURL(rawURL); err == nil {
			t.Errorf("ValidateFetchURL(%q) = nil, want error", rawURL)
		}
	}
}

func TestFetchInvalidURLDoesNotCallUpstream(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL)
	if _, err := c.Fetch(context.Background(), "sk-test", "file:///etc/passwd"); err == nil {
		t.Fatal("Fetch() = nil error, want validation error")
	}
	if calls.Load() != 0 {
		t.Errorf("upstream called %d times, want 0", calls.Load())
	}
}

func TestAPIErrors(t *testing.T) {
	tests := []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusPaymentRequired, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusRequestTimeout, true},
		{http.StatusRequestEntityTooLarge, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusGatewayTimeout, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, `{"error":{"message":"boom","type":"invalid_request_error","code":"invalid_request_error"}}`)
			}))
			defer upstream.Close()

			c := newTestClient(t, upstream.URL)
			_, err := c.Search(context.Background(), "sk-test", SearchRequest{Query: "golang"})
			if err == nil {
				t.Fatal("Search() = nil error, want APIError")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error type = %T (%v), want *APIError", err, err)
			}
			if apiErr.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tt.status)
			}
			if apiErr.Message != "boom" || apiErr.Type != "invalid_request_error" || apiErr.Code != "invalid_request_error" {
				t.Errorf("unexpected APIError: %+v", apiErr)
			}
			if apiErr.Retryable() != tt.retryable {
				t.Errorf("Retryable() = %v, want %v", apiErr.Retryable(), tt.retryable)
			}
			if !strings.Contains(apiErr.Error(), fmt.Sprintf("%d", tt.status)) {
				t.Errorf("Error() = %q, want status", apiErr.Error())
			}
		})
	}
}

func TestAPIErrorRedactsToken(t *testing.T) {
	const token = "sk-supersecret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid API key: `+token+`","type":"authentication_error","code":"invalid_api_key"}}`)
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL)
	_, err := c.Search(context.Background(), token, SearchRequest{Query: "golang"})
	if err == nil {
		t.Fatal("Search() = nil error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked token: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error = %v, want redaction marker", err)
	}
}

func TestNonJSONErrorBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "upstream gateway timeout\n")
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL)
	_, err := c.Search(context.Background(), "sk-test", SearchRequest{Query: "golang"})
	if err == nil {
		t.Fatal("Search() = nil error")
	}
	if !strings.Contains(err.Error(), "upstream gateway timeout") {
		t.Fatalf("error = %v, want upstream body snippet", err)
	}
}

func TestResponseTooLarge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 100))
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL, WithMaxResponseBytes(16))
	_, err := c.Search(context.Background(), "sk-test", SearchRequest{Query: "golang"})
	if err == nil {
		t.Fatal("Search() = nil error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error = %v, want size limit message", err)
	}
}

func TestTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		io.WriteString(w, `{"results":[]}`)
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL, WithTimeout(20*time.Millisecond))
	_, err := c.Search(context.Background(), "sk-test", SearchRequest{Query: "golang"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestMissingToken(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL)
	_, err := c.Search(context.Background(), "", SearchRequest{Query: "golang"})
	if err == nil || !strings.Contains(err.Error(), "missing DeepSeek API key") {
		t.Fatalf("error = %v, want missing key error", err)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream called %d times, want 0", calls.Load())
	}
}

func TestMalformedAndEmptyResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "malformed json", body: "not json", want: "decoding"},
		{name: "empty", body: "", want: "empty response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tt.body)
			}))
			defer upstream.Close()

			c := newTestClient(t, upstream.URL)
			_, err := c.Search(context.Background(), "sk-test", SearchRequest{Query: "golang"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNotFoundHintDependsOnEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"message":"not found"}}`)
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL)
	if _, err := c.Search(context.Background(), "sk-test", SearchRequest{Query: "golang"}); err == nil || !strings.Contains(err.Error(), "接口不存在") {
		t.Fatalf("search error = %v, want interface hint", err)
	}
	if _, err := c.Fetch(context.Background(), "sk-test", "https://example.com/a"); err == nil || !strings.Contains(err.Error(), "未获取到可用网页内容") {
		t.Fatalf("fetch error = %v, want no-content hint", err)
	}
}

func TestSearchValidation(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"results":[]}`)
	}))
	defer upstream.Close()

	c := newTestClient(t, upstream.URL)
	tests := []SearchRequest{
		{Query: "   "},
		{Query: strings.Repeat("a", 129)},
		{Query: "ok", Count: 51},
		{Query: "ok", Count: -1},
		{Query: "ok", OriQuestion: strings.Repeat("a", 129)},
	}
	for i, in := range tests {
		if _, err := c.Search(context.Background(), "sk-test", in); err == nil {
			t.Errorf("case %d: Search(%+v) = nil error, want validation error", i, in)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("upstream called %d times, want 0", calls.Load())
	}
}
