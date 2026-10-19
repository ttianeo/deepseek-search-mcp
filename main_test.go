package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"deepseek-search-mcp/internal/config"
	"deepseek-search-mcp/internal/deepseek"
	"deepseek-search-mcp/internal/mcpserver"
)

type upstreamState struct {
	mu         sync.Mutex
	searchAuth string
	searchBody deepseek.SearchRequest
	fetchAuth  string
	fetchBody  struct {
		URL string `json:"url"`
	}
	searchCalls int
	fetchCalls  int
}

func (s *upstreamState) snapshot() upstreamState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return upstreamState{
		searchAuth:  s.searchAuth,
		searchBody:  s.searchBody,
		fetchAuth:   s.fetchAuth,
		fetchBody:   s.fetchBody,
		searchCalls: s.searchCalls,
		fetchCalls:  s.fetchCalls,
	}
}

func newUpstream(t *testing.T) (*httptest.Server, *upstreamState) {
	t.Helper()
	state := &upstreamState{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case deepseek.SearchPath:
			var in deepseek.SearchRequest
			_ = json.Unmarshal(body, &in)
			state.mu.Lock()
			state.searchAuth = r.Header.Get("Authorization")
			state.searchBody = in
			state.searchCalls++
			state.mu.Unlock()

			if strings.Contains(in.Query, "fail401") {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"error":{"message":"invalid api key","type":"authentication_error","code":"invalid_api_key"}}`)
				return
			}

			site := "Example News"
			date := "2026-06-01T00:00:00+00:00"
			_ = json.NewEncoder(w).Encode(deepseek.SearchResponse{
				Results: []deepseek.SearchResult{{
					RerankScore: 2.4,
					Title:       "DeepSeek V4 模型发布",
					URL:         "https://example.com/deepseek-v4",
					Snippet:     "短摘要",
					Summary:     "长摘要",
					Content:     "网页正文",
					SiteName:    &site,
					Date:        &date,
					Authority:   3,
				}},
			})
		case deepseek.FetchPath:
			var in struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(body, &in)
			state.mu.Lock()
			state.fetchAuth = r.Header.Get("Authorization")
			state.fetchBody = in
			state.fetchCalls++
			state.mu.Unlock()
			_ = json.NewEncoder(w).Encode(deepseek.FetchResponse{
				Title:   "Example article",
				Content: "# Example article\n\nArticle content.",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream, state
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *headerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	for key, values := range t.headers {
		for _, value := range values {
			clone.Header.Add(key, value)
		}
	}
	return t.base.RoundTrip(clone)
}

func newTestApplication(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	return newTestApplicationWithConfig(t, upstreamURL, nil)
}

func newTestApplicationWithConfig(t *testing.T, upstreamURL string, mutate func(*config.Config)) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.DeepSeekBaseURL = upstreamURL
	cfg.RequestTimeout = 5 * time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	handler, err := newApplication(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newApplication: %v", err)
	}
	return handler
}

func connectClient(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "1.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{
			Transport: &headerRoundTripper{
				base: http.DefaultTransport,
				headers: http.Header{
					"Authorization": []string{"Bearer " + token},
				},
			},
		},
		DisableStandaloneSSE: true,
	}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connecting MCP client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("tool result has no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", res.Content[0])
	}
	return text.Text
}

func TestMCPEndToEnd(t *testing.T) {
	upstream, state := newUpstream(t)
	handler := newTestApplication(t, upstream.URL)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	const token = "sk-e2e-caller-token"
	session := connectClient(t, ts.URL+"/mcp", token)
	ctx := context.Background()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(tools.Tools))
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range tools.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{mcpserver.SearchToolName, mcpserver.FetchToolName} {
		if byName[name] == nil {
			t.Fatalf("missing tool %q", name)
		}
	}
	searchTool := byName[mcpserver.SearchToolName]
	if searchTool.Annotations == nil || !searchTool.Annotations.ReadOnlyHint {
		t.Errorf("search annotations = %+v, want ReadOnlyHint", searchTool.Annotations)
	}
	schemaBytes, err := json.Marshal(searchTool.InputSchema)
	if err != nil {
		t.Fatalf("marshaling input schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatalf("unmarshaling input schema: %v", err)
	}
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("required = %v, want [query]", required)
	}
	props, _ := schema["properties"].(map[string]any)
	countSchema, _ := props["count"].(map[string]any)
	if countSchema["maximum"] != float64(50) || countSchema["minimum"] != float64(1) {
		t.Errorf("count schema = %v", countSchema)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: mcpserver.SearchToolName,
		Arguments: map[string]any{
			"query":        "DeepSeek V4",
			"count":        5,
			"need_content": true,
		},
	})
	if err != nil {
		t.Fatalf("CallTool(search): %v", err)
	}
	if res.IsError {
		t.Fatalf("search returned tool error: %s", resultText(t, res))
	}
	text := resultText(t, res)
	for _, want := range []string{"DeepSeek 搜索结果", "DeepSeek V4 模型发布", "https://example.com/deepseek-v4", "网页正文"} {
		if !strings.Contains(text, want) {
			t.Errorf("search text missing %q:\n%s", want, text)
		}
	}

	var structured mcpserver.SearchOutput
	structuredBytes, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshaling structured content: %v", err)
	}
	if err := json.Unmarshal(structuredBytes, &structured); err != nil {
		t.Fatalf("unmarshaling structured content: %v", err)
	}
	if len(structured.Results) != 1 || structured.Results[0].Title != "DeepSeek V4 模型发布" {
		t.Fatalf("structured results = %+v", structured.Results)
	}

	snap := state.snapshot()
	if snap.searchAuth != "Bearer "+token {
		t.Errorf("upstream search Authorization = %q", snap.searchAuth)
	}
	if snap.searchBody.Query != "DeepSeek V4" || snap.searchBody.Count != 5 || !snap.searchBody.NeedContent {
		t.Errorf("upstream search body = %+v", snap.searchBody)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: mcpserver.SearchToolName,
		Arguments: map[string]any{
			"query": "defaults",
		},
	})
	if err != nil {
		t.Fatalf("CallTool(search defaults): %v", err)
	}
	if res.IsError {
		t.Fatalf("search with defaults returned tool error: %s", resultText(t, res))
	}
	snap = state.snapshot()
	if snap.searchBody.Count != 10 {
		t.Errorf("default count = %d, want 10", snap.searchBody.Count)
	}
	if snap.searchBody.NeedContent {
		t.Error("default need_content = true, want false")
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: mcpserver.FetchToolName,
		Arguments: map[string]any{
			"url": "https://example.com/article",
		},
	})
	if err != nil {
		t.Fatalf("CallTool(fetch): %v", err)
	}
	if res.IsError {
		t.Fatalf("fetch returned tool error: %s", resultText(t, res))
	}
	if text := resultText(t, res); !strings.Contains(text, "# Example article") || !strings.Contains(text, "Article content") {
		t.Errorf("fetch text = %q", text)
	}
	snap = state.snapshot()
	if snap.fetchAuth != "Bearer "+token {
		t.Errorf("upstream fetch Authorization = %q", snap.fetchAuth)
	}
	if snap.fetchBody.URL != "https://example.com/article" {
		t.Errorf("upstream fetch body = %+v", snap.fetchBody)
	}
}

func TestMCPStatefulSession(t *testing.T) {
	upstream, state := newUpstream(t)
	handler := newTestApplicationWithConfig(t, upstream.URL, func(c *config.Config) {
		c.Stateless = false
		c.SessionTimeout = time.Minute
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	session := connectClient(t, ts.URL+"/mcp", "sk-stateful-token")
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: mcpserver.SearchToolName,
		Arguments: map[string]any{
			"query": "stateful",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("stateful search returned tool error: %s", resultText(t, res))
	}
	if got := state.snapshot().searchAuth; got != "Bearer sk-stateful-token" {
		t.Errorf("upstream Authorization = %q", got)
	}
}

func TestMCPInvalidArguments(t *testing.T) {
	upstream, state := newUpstream(t)
	handler := newTestApplication(t, upstream.URL)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	session := connectClient(t, ts.URL+"/mcp", "sk-test")
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: mcpserver.SearchToolName,
		Arguments: map[string]any{
			"query": "ok",
			"count": 51,
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("count=51 should be a tool error")
	}
	if text := resultText(t, res); !strings.Contains(text, "count") {
		t.Errorf("error text = %q, want count validation", text)
	}
	if state.snapshot().searchCalls != 0 {
		t.Error("upstream should not be called for invalid arguments")
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: mcpserver.SearchToolName,
		Arguments: map[string]any{
			"query":        "ok",
			"localization": "not-an-ip",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("invalid localization should be a tool error")
	}
	if state.snapshot().searchCalls != 0 {
		t.Error("upstream should not be called for an invalid localization")
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: mcpserver.FetchToolName,
		Arguments: map[string]any{
			"url": "file:///etc/passwd",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("file:// URL should be a tool error")
	}
	if state.snapshot().fetchCalls != 0 {
		t.Error("upstream should not be called for an invalid fetch URL")
	}
}

func TestMCPUpstreamAuthError(t *testing.T) {
	upstream, _ := newUpstream(t)
	handler := newTestApplication(t, upstream.URL)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	session := connectClient(t, ts.URL+"/mcp", "sk-bad")
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: mcpserver.SearchToolName,
		Arguments: map[string]any{
			"query": "fail401",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("upstream 401 should be a tool error")
	}
	text := resultText(t, res)
	for _, want := range []string{"401", "Token"} {
		if !strings.Contains(text, want) {
			t.Errorf("error text = %q, want %q", text, want)
		}
	}
}

func TestHTTPRequiresBearerToken(t *testing.T) {
	upstream, state := newUpstream(t)
	handler := newTestApplication(t, upstream.URL)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("WWW-Authenticate = %q", resp.Header.Get("WWW-Authenticate"))
	}
	if state.snapshot().searchCalls != 0 || state.snapshot().fetchCalls != 0 {
		t.Error("upstream must not be called without a caller token")
	}
}
