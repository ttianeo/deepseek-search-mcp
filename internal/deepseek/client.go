// Package deepseek implements a minimal client for the DeepSeek Search and
// Fetch tool APIs.
//
// See https://api-docs.deepseek.com/zh-cn/guides/search.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultBaseURL is the public DeepSeek API endpoint.
	DefaultBaseURL = "https://api.deepseek.com"
	// SearchPath is the search tool endpoint.
	SearchPath = "/v1/tools/search"
	// FetchPath is the fetch tool endpoint.
	FetchPath = "/v1/tools/fetch"

	// DefaultTimeout bounds a single upstream request.
	DefaultTimeout = 60 * time.Second
	// DefaultMaxResponseBytes bounds the size of an upstream response body.
	DefaultMaxResponseBytes int64 = 32 << 20
)

// Client calls the DeepSeek Search and Fetch APIs with a caller-supplied token.
type Client struct {
	baseURL          string
	httpClient       *http.Client
	timeout          time.Duration
	maxResponseBytes int64
	userAgent        string
}

// Option configures a Client.
type Option func(*Client) error

// WithHTTPClient sets the HTTP client used for upstream requests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		if hc == nil {
			return errors.New("http client must not be nil")
		}
		c.httpClient = hc
		return nil
	}
}

// WithTimeout bounds a single upstream request.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) error {
		if d <= 0 {
			return fmt.Errorf("timeout must be positive, got %s", d)
		}
		c.timeout = d
		return nil
	}
}

// WithMaxResponseBytes bounds the size of an upstream response body.
func WithMaxResponseBytes(n int64) Option {
	return func(c *Client) error {
		if n <= 0 {
			return fmt.Errorf("max response bytes must be positive, got %d", n)
		}
		c.maxResponseBytes = n
		return nil
	}
}

// WithUserAgent sets the User-Agent sent to DeepSeek.
func WithUserAgent(ua string) Option {
	return func(c *Client) error {
		c.userAgent = ua
		return nil
	}
}

// New creates a Client for baseURL. An empty baseURL selects DefaultBaseURL.
func New(baseURL string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")

	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid DeepSeek base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid DeepSeek base URL %q: scheme must be http or https", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid DeepSeek base URL %q: missing host", baseURL)
	}
	if u.User != nil {
		return nil, errors.New("invalid DeepSeek base URL: credentials must not be embedded in the URL")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid DeepSeek base URL: query and fragment are not supported")
	}

	c := &Client{
		baseURL:          baseURL,
		timeout:          DefaultTimeout,
		maxResponseBytes: DefaultMaxResponseBytes,
		userAgent:        "deepseek-search-mcp",
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Transport: defaultTransport()}
	}
	return c, nil
}

func defaultTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// SearchRequest is the request body for POST /v1/tools/search.
//
// Zero values for Count and NeedContent are omitted so the DeepSeek defaults
// (10 and false) apply.
type SearchRequest struct {
	Query        string `json:"query"`
	Count        int    `json:"count,omitempty"`
	OriQuestion  string `json:"ori_question,omitempty"`
	Localization string `json:"localization,omitempty"`
	NeedContent  bool   `json:"need_content,omitempty"`
}

// SearchResponse is the response body for POST /v1/tools/search.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

// SearchResult is a single search result.
type SearchResult struct {
	RerankScore float64 `json:"rerank_score"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	Content     string  `json:"content"`
	SiteName    *string `json:"site_name"`
	SiteIcon    *string `json:"site_icon"`
	Date        *string `json:"date"`
	Authority   int     `json:"authority"`
}

// FetchResponse is the response body for POST /v1/tools/fetch.
type FetchResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// APIError is an error response returned by the DeepSeek API.
type APIError struct {
	// StatusCode is the HTTP status returned by DeepSeek.
	StatusCode int
	// Type and Code mirror the OpenAI-compatible error object.
	Type string
	Code string
	// Message is the upstream error message, with the caller's token redacted.
	Message string
	// RequestID is the upstream request ID when DeepSeek provides one.
	RequestID string

	path string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("DeepSeek API 返回 %d %s", e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if hint := statusHint(e.StatusCode, e.path); hint != "" {
		msg += "（" + hint + "）"
	}
	if e.RequestID != "" {
		msg += " [request_id=" + e.RequestID + "]"
	}
	return msg
}

// Retryable reports whether the request may succeed if retried after a delay.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusRequestTimeout ||
		e.StatusCode == http.StatusTooManyRequests ||
		e.StatusCode >= 500
}

func statusHint(status int, path string) string {
	switch status {
	case http.StatusBadRequest:
		return "请求参数不合法"
	case http.StatusUnauthorized:
		return "Token 缺失或无效，请检查 Authorization 头"
	case http.StatusPaymentRequired:
		return "DeepSeek 账户余额不足，请充值后重试"
	case http.StatusForbidden:
		return "请求被 DeepSeek 安全策略拒绝"
	case http.StatusNotFound:
		if path == FetchPath {
			return "未获取到可用网页内容（URL 不可用、被策略拦截、站点报错或无可提取正文）"
		}
		return "接口不存在，请检查 DEEPSEEK_BASE_URL"
	case http.StatusRequestTimeout:
		return "读取请求体超时，可稍后重试"
	case http.StatusRequestEntityTooLarge:
		return "请求体超过大小限制"
	case http.StatusTooManyRequests:
		return "触发 DeepSeek QPS 限流，请稍后重试"
	case http.StatusInternalServerError:
		return "DeepSeek 服务内部错误，可稍后重试"
	case http.StatusGatewayTimeout:
		return "DeepSeek 下游请求超时，可稍后重试"
	default:
		return ""
	}
}

type apiErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Search calls the DeepSeek search tool with token.
func (c *Client) Search(ctx context.Context, token string, in SearchRequest) (*SearchResponse, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, errors.New("query must not be empty")
	}
	if n := utf8.RuneCountInString(in.Query); n > 128 {
		return nil, fmt.Errorf("query must be at most 128 characters, got %d", n)
	}
	if in.Count < 0 || in.Count > 50 {
		return nil, fmt.Errorf("count must be between 1 and 50, got %d", in.Count)
	}
	if in.OriQuestion != "" && utf8.RuneCountInString(in.OriQuestion) > 128 {
		return nil, errors.New("ori_question must be at most 128 characters")
	}

	var out SearchResponse
	if err := c.post(ctx, token, SearchPath, in, &out); err != nil {
		return nil, err
	}
	if out.Results == nil {
		out.Results = []SearchResult{}
	}
	return &out, nil
}

// Fetch calls the DeepSeek fetch tool for rawURL with token.
func (c *Client) Fetch(ctx context.Context, token, rawURL string) (*FetchResponse, error) {
	if err := ValidateFetchURL(rawURL); err != nil {
		return nil, err
	}
	var out FetchResponse
	if err := c.post(ctx, token, FetchPath, struct {
		URL string `json:"url"`
	}{URL: rawURL}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ValidateFetchURL checks the constraints documented for the fetch endpoint.
// It performs no network access.
func ValidateFetchURL(rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return errors.New("url must not be empty")
	}
	if len(rawURL) > 8192 {
		return fmt.Errorf("url must be at most 8192 bytes, got %d", len(rawURL))
	}
	if strings.IndexFunc(rawURL, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return errors.New("url must not contain whitespace or control characters")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("url must include a host")
	}
	if u.User != nil {
		return errors.New("url must not include user information")
	}
	return nil
}

func (c *Client) post(ctx context.Context, token, path string, in, out any) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("missing DeepSeek API key")
	}

	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating upstream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling DeepSeek %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.ContentLength > c.maxResponseBytes {
		return fmt.Errorf("DeepSeek %s response is too large (%d bytes, limit %d)", path, resp.ContentLength, c.maxResponseBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("reading DeepSeek %s response: %w", path, err)
	}
	if int64(len(body)) > c.maxResponseBytes {
		return fmt.Errorf("DeepSeek %s response exceeded the %d byte limit", path, c.maxResponseBytes)
	}

	if resp.StatusCode != http.StatusOK {
		return parseAPIError(resp, body, token, path)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("DeepSeek %s returned an empty response", path)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding DeepSeek %s response: %w", path, err)
	}
	return nil
}

func parseAPIError(resp *http.Response, body []byte, token, path string) *APIError {
	e := &APIError{
		StatusCode: resp.StatusCode,
		RequestID:  resp.Header.Get("X-Request-Id"),
		path:       path,
	}

	var envelope apiErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil &&
		(envelope.Error.Message != "" || envelope.Error.Type != "" || envelope.Error.Code != "") {
		e.Message = envelope.Error.Message
		e.Type = envelope.Error.Type
		e.Code = envelope.Error.Code
	} else {
		e.Message = snippet(body, 512)
	}

	e.Message = redact(e.Message, token)
	if len(e.Message) > 2000 {
		e.Message = e.Message[:2000] + "…"
	}
	return e
}

func snippet(body []byte, limit int) string {
	if len(body) > limit {
		body = body[:limit]
	}
	s := strings.ToValidUTF8(string(body), "�")
	return strings.Join(strings.Fields(s), " ")
}

func redact(message, token string) string {
	if token == "" {
		return message
	}
	message = strings.ReplaceAll(message, "Bearer "+token, "Bearer [REDACTED]")
	return strings.ReplaceAll(message, token, "[REDACTED]")
}
