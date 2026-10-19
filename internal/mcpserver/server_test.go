package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"deepseek-search-mcp/internal/auth"
	"deepseek-search-mcp/internal/deepseek"
)

func TestSearchInputSchema(t *testing.T) {
	s, err := buildSearchInputSchema()
	if err != nil {
		t.Fatalf("buildSearchInputSchema: %v", err)
	}
	if len(s.Required) != 1 || s.Required[0] != "query" {
		t.Errorf("required = %v, want [query]", s.Required)
	}
	q := s.Properties["query"]
	if q == nil || q.MinLength == nil || *q.MinLength != 1 || q.MaxLength == nil || *q.MaxLength != 128 {
		t.Errorf("query schema = %+v", q)
	}
	if q.Pattern != `\S` {
		t.Errorf("query pattern = %q", q.Pattern)
	}
	c := s.Properties["count"]
	if c.Minimum == nil || *c.Minimum != 1 || c.Maximum == nil || *c.Maximum != 50 {
		t.Errorf("count schema = %+v", c)
	}
	if string(c.Default) != "10" {
		t.Errorf("count default = %s, want 10", c.Default)
	}
	if got := string(s.Properties["need_content"].Default); got != "false" {
		t.Errorf("need_content default = %s, want false", got)
	}
	if s.AdditionalProperties == nil {
		t.Error("search schema should reject additional properties")
	}
}

func TestFetchInputSchema(t *testing.T) {
	s, err := buildFetchInputSchema()
	if err != nil {
		t.Fatalf("buildFetchInputSchema: %v", err)
	}
	if len(s.Required) != 1 || s.Required[0] != "url" {
		t.Errorf("required = %v, want [url]", s.Required)
	}
	u := s.Properties["url"]
	if u.MinLength == nil || *u.MinLength != 1 || u.MaxLength == nil || *u.MaxLength != 8192 {
		t.Errorf("url schema = %+v", u)
	}
	if !strings.EqualFold(u.Pattern, `(?i)^https?://`) {
		t.Errorf("url pattern = %q", u.Pattern)
	}
}

func TestValidateSearchInput(t *testing.T) {
	valid := []SearchInput{
		{Query: "golang"},
		{Query: "golang", Count: 10},
		{Query: "golang", Count: 50},
		{Query: "golang", OriQuestion: "原始问题"},
		{Query: "golang", Localization: "203.0.113.10"},
		{Query: "golang", Localization: "2001:db8::1"},
	}
	for _, in := range valid {
		if err := validateSearchInput(in); err != nil {
			t.Errorf("validateSearchInput(%+v) = %v, want nil", in, err)
		}
	}

	invalid := []SearchInput{
		{Query: ""},
		{Query: "   "},
		{Query: strings.Repeat("a", 129)},
		{Query: "ok", Count: 51},
		{Query: "ok", Count: -1},
		{Query: "ok", OriQuestion: strings.Repeat("a", 129)},
		{Query: "ok", Localization: "not-an-ip"},
	}
	for _, in := range invalid {
		if err := validateSearchInput(in); err == nil {
			t.Errorf("validateSearchInput(%+v) = nil, want error", in)
		}
	}
}

func TestToSearchOutput(t *testing.T) {
	site := "Example"
	icon := "https://example.com/icon.png"
	date := "2026-06-01T00:00:00+00:00"
	resp := &deepseek.SearchResponse{Results: []deepseek.SearchResult{{
		RerankScore: 2.4,
		Title:       "title",
		URL:         "https://example.com",
		Snippet:     "snippet",
		Summary:     "summary",
		Content:     "content",
		SiteName:    &site,
		SiteIcon:    &icon,
		Date:        &date,
		Authority:   3,
	}}}
	out := toSearchOutput(resp)
	if len(out.Results) != 1 {
		t.Fatalf("len(results) = %d", len(out.Results))
	}
	r := out.Results[0]
	if r.SiteName != site || r.SiteIcon != icon || r.Date != date || r.Authority != 3 {
		t.Errorf("unexpected result: %+v", r)
	}

	empty := toSearchOutput(&deepseek.SearchResponse{})
	if empty.Results == nil {
		t.Fatal("empty response should produce a non-nil slice")
	}
	if len(empty.Results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(empty.Results))
	}
}

func TestRenderSearch(t *testing.T) {
	out := SearchOutput{Results: []SearchResult{{
		RerankScore: 2.4,
		Title:       "DeepSeek V4",
		URL:         "https://example.com/a",
		Summary:     "摘要文本",
		Content:     "正文文本",
		SiteName:    "Example",
		Date:        "2026-06-01T00:00:00+00:00",
		Authority:   3,
	}}}
	text := renderSearch("DeepSeek V4", out)
	for _, want := range []string{"DeepSeek 搜索结果", "查询：DeepSeek V4", "共 1 条结果", "## 1. DeepSeek V4", "摘要文本", "正文文本", "权威性: 3"} {
		if !strings.Contains(text, want) {
			t.Errorf("renderSearch output missing %q:\n%s", want, text)
		}
	}

	empty := renderSearch("nothing", SearchOutput{Results: []SearchResult{}})
	if !strings.Contains(empty, "未找到相关结果") {
		t.Errorf("empty render = %q", empty)
	}
}

func TestRenderFetch(t *testing.T) {
	text := renderFetch(FetchOutput{URL: "https://example.com/a", Title: "标题", Content: "正文"})
	for _, want := range []string{"# 标题", "来源：https://example.com/a", "正文"} {
		if !strings.Contains(text, want) {
			t.Errorf("renderFetch output missing %q:\n%s", want, text)
		}
	}

	empty := renderFetch(FetchOutput{URL: "https://example.com/a"})
	if !strings.Contains(empty, "未返回可提取的正文内容") {
		t.Errorf("empty render = %q", empty)
	}
}

func TestTokenFromRequest(t *testing.T) {
	ctx := context.Background()
	headerReq := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{
		"Authorization": []string{"Bearer sk-header"},
	}}}
	token, err := tokenFromRequest(ctx, headerReq)
	if err != nil || token != "sk-header" {
		t.Fatalf("token = %q, err = %v", token, err)
	}

	ctx = auth.WithToken(ctx, "sk-context")
	token, err = tokenFromRequest(ctx, nil)
	if err != nil || token != "sk-context" {
		t.Fatalf("fallback token = %q, err = %v", token, err)
	}

	malformed := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{
		"Authorization": []string{"Basic abc"},
	}}}
	if _, err := tokenFromRequest(context.Background(), malformed); err == nil {
		t.Fatal("malformed header should return an error")
	}

	if _, err := tokenFromRequest(context.Background(), nil); err == nil {
		t.Fatal("missing header and context token should return an error")
	}
}
