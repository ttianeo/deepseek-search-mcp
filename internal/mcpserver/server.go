// Package mcpserver exposes DeepSeek Search and Fetch as MCP tools.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"deepseek-search-mcp/internal/auth"
	"deepseek-search-mcp/internal/deepseek"
)

const (
	// ServerName is the MCP implementation name.
	ServerName = "deepseek-search-mcp"
	// SearchToolName is the tool that calls POST /v1/tools/search.
	SearchToolName = "search"
	// FetchToolName is the tool that calls POST /v1/tools/fetch.
	FetchToolName = "fetch"
)

// DeepSeek is the upstream API surface used by the tools.
type DeepSeek interface {
	Search(ctx context.Context, token string, in deepseek.SearchRequest) (*deepseek.SearchResponse, error)
	Fetch(ctx context.Context, token, rawURL string) (*deepseek.FetchResponse, error)
}

// SearchInput is the tool input for search.
type SearchInput struct {
	Query        string `json:"query" jsonschema:"搜索词（1-128 个字符），不能全为空白。支持 site:github.com、-site:example.com、lang:zh,en、after:2025-01-01、before:2026、\"精确短语\"、-排除词 等搜索语法。"`
	Count        int    `json:"count,omitempty" jsonschema:"返回结果条数，范围 1-50，默认 10。"`
	OriQuestion  string `json:"ori_question,omitempty" jsonschema:"用户的原始问题（最多 128 个字符），用于帮助理解搜索意图。"`
	Localization string `json:"localization,omitempty" jsonschema:"用于优先返回本地化结果的 IP 地址，例如 203.0.113.10；不确定时省略。"`
	NeedContent  bool   `json:"need_content,omitempty" jsonschema:"为 true 时在每条结果的 content 字段返回网页正文；默认 false。开启后响应更大、延迟更高。"`
}

// FetchInput is the tool input for fetch.
type FetchInput struct {
	URL string `json:"url" jsonschema:"要抓取的网页地址，必须是 HTTP/HTTPS URL，最长 8192 字节，不能包含用户名或密码。"`
}

// SearchOutput is the structured output for search.
type SearchOutput struct {
	Results []SearchResult `json:"results" jsonschema:"搜索结果列表，按相关性从高到低排列；无结果时为空数组。"`
}

// SearchResult is one structured search result.
type SearchResult struct {
	RerankScore float64 `json:"rerank_score" jsonschema:"排序分数（0-3），综合相关性、权威性与时效性；不低于 1.5 通常为优质结果。"`
	Title       string  `json:"title" jsonschema:"网页标题。"`
	URL         string  `json:"url" jsonschema:"网页链接。"`
	Snippet     string  `json:"snippet,omitempty" jsonschema:"约 100-200 字符的短摘要，适合展示。"`
	Summary     string  `json:"summary,omitempty" jsonschema:"约 600-800 字符的长摘要，适合模型阅读。"`
	Content     string  `json:"content,omitempty" jsonschema:"网页正文；仅在 need_content 为 true 且正文可用时返回。"`
	SiteName    string  `json:"site_name,omitempty" jsonschema:"站点名称。"`
	SiteIcon    string  `json:"site_icon,omitempty" jsonschema:"站点图标 URL。"`
	Date        string  `json:"date,omitempty" jsonschema:"发布时间，UTC 的 RFC 3339 格式；未知时省略。"`
	Authority   int     `json:"authority" jsonschema:"网页权威性（0-4，-1 表示未知）；不低于 3 为极权威。"`
}

// FetchOutput is the structured output for fetch.
type FetchOutput struct {
	URL     string `json:"url" jsonschema:"实际抓取的网页地址。"`
	Title   string `json:"title" jsonschema:"页面标题；缺失时为空字符串。"`
	Content string `json:"content" jsonschema:"抓取并转换为 Markdown 的网页正文。"`
}

const serverInstructions = `本服务把调用方 Authorization 头中的 DeepSeek API Key 透传给 DeepSeek Search/Fetch API，不存储、不记录 Token。
- search：检索互联网，支持 site:、lang:、after:、before:、精确短语和排除语法；need_content=true 时返回网页正文。
- fetch：抓取单个 HTTP/HTTPS 网页并返回 Markdown 正文。
调用工具时无需在参数中传递 API Key，鉴权由 MCP 连接头完成。`

const searchDescription = `使用 DeepSeek 搜索服务检索互联网，返回带相关性评分、权威性、发布日期和摘要的结果。
支持搜索语法：site:github.com、-site:example.com、lang:zh,en、after:2025-01-01、before:2026、"精确短语"、-排除词。
需要阅读网页正文时把 need_content 设为 true（响应更大、延迟更高）。`

const fetchDescription = `使用 DeepSeek 抓取服务读取指定 HTTP/HTTPS 网页，并返回 Markdown 正文。
仅支持公开网页；目标站点可能拒绝访问，或页面没有可提取的正文（此时 DeepSeek 返回 404）。`

// New builds the MCP server with the search and fetch tools.
func New(client DeepSeek, version string, logger *slog.Logger) (*mcp.Server, error) {
	if client == nil {
		return nil, errors.New("deepseek client must not be nil")
	}
	if version == "" {
		version = "dev"
	}

	searchInput, err := buildSearchInputSchema()
	if err != nil {
		return nil, err
	}
	fetchInput, err := buildFetchInputSchema()
	if err != nil {
		return nil, err
	}
	searchOutput, err := jsonschema.For[SearchOutput](nil)
	if err != nil {
		return nil, fmt.Errorf("building search output schema: %w", err)
	}
	fetchOutput, err := jsonschema.For[FetchOutput](nil)
	if err != nil {
		return nil, fmt.Errorf("building fetch output schema: %w", err)
	}

	s := mcp.NewServer(&mcp.Implementation{
		Name:        ServerName,
		Title:       "DeepSeek Search",
		Description: "DeepSeek 搜索与网页抓取 MCP 服务（调用方自带 API Key）",
		Version:     version,
		WebsiteURL:  "https://api-docs.deepseek.com/zh-cn/guides/search",
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
		Logger:       logger,
	})

	h := &handlers{client: client, logger: logger}

	mcp.AddTool(s, &mcp.Tool{
		Name:         SearchToolName,
		Title:        "DeepSeek 搜索",
		Description:  searchDescription,
		InputSchema:  searchInput,
		OutputSchema: searchOutput,
		Annotations:  readOnlyAnnotations("DeepSeek 搜索"),
	}, h.search)

	mcp.AddTool(s, &mcp.Tool{
		Name:         FetchToolName,
		Title:        "DeepSeek 网页抓取",
		Description:  fetchDescription,
		InputSchema:  fetchInput,
		OutputSchema: fetchOutput,
		Annotations:  readOnlyAnnotations("DeepSeek 网页抓取"),
	}, h.fetch)

	return s, nil
}

func readOnlyAnnotations(title string) *mcp.ToolAnnotations {
	openWorld := true
	return &mcp.ToolAnnotations{
		Title:          title,
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  &openWorld,
	}
}

func buildSearchInputSchema() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[SearchInput](nil)
	if err != nil {
		return nil, fmt.Errorf("building search input schema: %w", err)
	}

	query := s.Properties["query"]
	query.MinLength = intPtr(1)
	query.MaxLength = intPtr(128)
	query.Pattern = `\S`

	count := s.Properties["count"]
	count.Minimum = floatPtr(1)
	count.Maximum = floatPtr(50)
	count.Default = json.RawMessage("10")

	s.Properties["ori_question"].MaxLength = intPtr(128)
	s.Properties["need_content"].Default = json.RawMessage("false")

	return s, nil
}

func buildFetchInputSchema() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[FetchInput](nil)
	if err != nil {
		return nil, fmt.Errorf("building fetch input schema: %w", err)
	}

	u := s.Properties["url"]
	u.MinLength = intPtr(1)
	u.MaxLength = intPtr(8192)
	u.Format = "uri"
	u.Pattern = `(?i)^https?://`

	return s, nil
}

type handlers struct {
	client DeepSeek
	logger *slog.Logger
}

func (h *handlers) search(ctx context.Context, req *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	token, err := tokenFromRequest(ctx, req)
	if err != nil {
		return nil, SearchOutput{}, err
	}
	if err := validateSearchInput(in); err != nil {
		return nil, SearchOutput{}, err
	}

	count := in.Count
	if count == 0 {
		count = 10
	}
	query := strings.TrimSpace(in.Query)

	resp, err := h.client.Search(ctx, token, deepseek.SearchRequest{
		Query:        query,
		Count:        count,
		OriQuestion:  strings.TrimSpace(in.OriQuestion),
		Localization: strings.TrimSpace(in.Localization),
		NeedContent:  in.NeedContent,
	})
	if err != nil {
		h.logFailure(SearchToolName, err)
		return nil, SearchOutput{}, fmt.Errorf("搜索请求失败: %w", err)
	}

	out := toSearchOutput(resp)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderSearch(query, out)}},
	}, out, nil
}

func (h *handlers) fetch(ctx context.Context, req *mcp.CallToolRequest, in FetchInput) (*mcp.CallToolResult, FetchOutput, error) {
	token, err := tokenFromRequest(ctx, req)
	if err != nil {
		return nil, FetchOutput{}, err
	}

	rawURL := strings.TrimSpace(in.URL)
	if err := deepseek.ValidateFetchURL(rawURL); err != nil {
		return nil, FetchOutput{}, fmt.Errorf("url 参数不合法: %w", err)
	}

	resp, err := h.client.Fetch(ctx, token, rawURL)
	if err != nil {
		h.logFailure(FetchToolName, err)
		return nil, FetchOutput{}, fmt.Errorf("网页抓取失败: %w", err)
	}

	out := FetchOutput{URL: rawURL, Title: resp.Title, Content: resp.Content}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: renderFetch(out)}},
	}, out, nil
}

func validateSearchInput(in SearchInput) error {
	n := utf8.RuneCountInString(in.Query)
	if n == 0 {
		return errors.New("query 不能为空")
	}
	if n > 128 {
		return fmt.Errorf("query 最多 128 个字符，当前 %d 个", n)
	}
	if strings.TrimSpace(in.Query) == "" {
		return errors.New("query 不能全为空白")
	}
	if in.Count < 0 || in.Count > 50 {
		return fmt.Errorf("count 必须在 1-50 之间，当前为 %d", in.Count)
	}
	if in.OriQuestion != "" && utf8.RuneCountInString(in.OriQuestion) > 128 {
		return errors.New("ori_question 最多 128 个字符")
	}
	if in.Localization != "" && net.ParseIP(in.Localization) == nil {
		return fmt.Errorf("localization 必须是合法的 IP 地址，当前为 %q", in.Localization)
	}
	return nil
}

func tokenFromRequest(ctx context.Context, req *mcp.CallToolRequest) (string, error) {
	if req != nil && req.Extra != nil && req.Extra.Header != nil {
		token, err := auth.FromHeader(req.Extra.Header)
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, auth.ErrMissing) {
			return "", errors.New(`Authorization 头无效：期望格式为 "Bearer <DeepSeek API Key>"`)
		}
	}
	if token, ok := auth.FromContext(ctx); ok {
		return token, nil
	}
	return "", errors.New(`未提供 DeepSeek API Key：请在 MCP 连接头中设置 "Authorization: Bearer <DeepSeek API Key>"`)
}

func (h *handlers) logFailure(tool string, err error) {
	if h.logger != nil {
		h.logger.Warn("DeepSeek tool call failed", "tool", tool, "error", err)
	}
}

func toSearchOutput(resp *deepseek.SearchResponse) SearchOutput {
	results := make([]SearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		sr := SearchResult{
			RerankScore: r.RerankScore,
			Title:       r.Title,
			URL:         r.URL,
			Snippet:     r.Snippet,
			Summary:     r.Summary,
			Content:     r.Content,
			Authority:   r.Authority,
		}
		if r.SiteName != nil {
			sr.SiteName = *r.SiteName
		}
		if r.SiteIcon != nil {
			sr.SiteIcon = *r.SiteIcon
		}
		if r.Date != nil {
			sr.Date = *r.Date
		}
		results = append(results, sr)
	}
	return SearchOutput{Results: results}
}

func renderSearch(query string, out SearchOutput) string {
	var b strings.Builder
	b.WriteString("# DeepSeek 搜索结果\n\n")
	fmt.Fprintf(&b, "查询：%s\n\n", query)

	if len(out.Results) == 0 {
		b.WriteString("未找到相关结果。可以尝试更换关键词，或使用 site:、lang:、after:、before: 等搜索语法。\n")
		return b.String()
	}

	fmt.Fprintf(&b, "共 %d 条结果。\n\n", len(out.Results))
	for i, r := range out.Results {
		title := r.Title
		if title == "" {
			title = r.URL
		}
		fmt.Fprintf(&b, "## %d. %s\n\n", i+1, title)
		fmt.Fprintf(&b, "- URL: %s\n", r.URL)
		fmt.Fprintf(&b, "- 相关性评分: %.2f\n", r.RerankScore)
		fmt.Fprintf(&b, "- 权威性: %d\n", r.Authority)
		if r.Date != "" {
			fmt.Fprintf(&b, "- 发布时间: %s\n", r.Date)
		}
		if r.SiteName != "" {
			fmt.Fprintf(&b, "- 站点: %s\n", r.SiteName)
		}

		if r.Summary != "" {
			fmt.Fprintf(&b, "\n**摘要**：%s\n", r.Summary)
		} else if r.Snippet != "" {
			fmt.Fprintf(&b, "\n**摘要**：%s\n", r.Snippet)
		}
		if r.Content != "" {
			fmt.Fprintf(&b, "\n**正文**：\n\n%s\n", r.Content)
		}
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}

func renderFetch(out FetchOutput) string {
	var b strings.Builder
	if out.Title != "" {
		fmt.Fprintf(&b, "# %s\n\n", out.Title)
	}
	fmt.Fprintf(&b, "来源：%s\n\n", out.URL)
	if out.Content != "" {
		b.WriteString(out.Content)
		b.WriteString("\n")
	} else {
		b.WriteString("未返回可提取的正文内容。\n")
	}
	return b.String()
}

func intPtr(v int) *int {
	return &v
}

func floatPtr(v float64) *float64 {
	return &v
}
