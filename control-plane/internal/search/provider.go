// Package search provides the control-plane-side web search providers
// for the BT-2 search proxy (ADR-0012 §3). Provider API keys live ONLY
// here, injected from control-plane secrets — the agent runtime never
// sees them; it calls POST /api/v1/tools/web_search instead.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Result is one search hit in the contract shape (ADR-0012 §2).
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Provider executes a web search. Implementations must honour ctx for
// cancellation/timeout.
type Provider interface {
	Search(ctx context.Context, query string, maxResults int) ([]Result, error)
}

// maxResponseBytes caps how much of a provider response body we buffer.
const maxResponseBytes = 4 << 20 // 4 MiB

// --- DuckDuckGo (keyless default) -------------------------------------

// DuckDuckGo scrapes the keyless HTML endpoint
// (https://html.duckduckgo.com/html/?q=...). DDG offers no free JSON
// API, so the HTML surface is the contract-stable option pinned by
// ADR-0012.
type DuckDuckGo struct {
	HTTPClient *http.Client
	BaseURL    string // override for tests
}

func NewDuckDuckGo(client *http.Client) *DuckDuckGo {
	if client == nil {
		client = http.DefaultClient
	}
	return &DuckDuckGo{HTTPClient: client, BaseURL: "https://html.duckduckgo.com/html/"}
}

var (
	ddgTitleRe   = regexp.MustCompile(`(?is)<a[^>]*class="[^"]*\bresult__a\b[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?is)<a[^>]*class="[^"]*\bresult__snippet\b[^"]*"[^>]*>(.*?)</a>`)
	htmlTagRe    = regexp.MustCompile(`<[^>]*>`)
)

func (d *DuckDuckGo) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	endpoint := strings.TrimSuffix(d.BaseURL, "/") + "/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; skquad-control-plane/1.0)")
	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: read body: %w", err)
	}
	return ParseDuckDuckGoHTML(string(body), maxResults), nil
}

// ParseDuckDuckGoHTML extracts up to maxResults (title, url, snippet)
// triples from the html.duckduckgo.com HTML layout. Snippets pair with
// the title block that precedes them.
func ParseDuckDuckGoHTML(body string, maxResults int) []Result {
	titles := ddgTitleRe.FindAllStringSubmatchIndex(body, -1)
	snips := ddgSnippetRe.FindAllStringSubmatchIndex(body, -1)
	results := make([]Result, 0, maxResults)
	si := 0
	for ti := 0; ti < len(titles) && len(results) < maxResults; ti++ {
		t := titles[ti]
		href := cleanText(body[t[2]:t[3]])
		title := cleanText(body[t[4]:t[5]])
		if href == "" || title == "" {
			continue
		}
		r := Result{Title: title, URL: unwrapDDGHref(href)}
		// Advance to the first snippet after this title's start.
		for si < len(snips) && snips[si][0] < t[0] {
			si++
		}
		if si < len(snips) {
			nextTitleStart := len(body)
			if ti+1 < len(titles) {
				nextTitleStart = titles[ti+1][0]
			}
			if snips[si][0] < nextTitleStart {
				r.Snippet = cleanText(body[snips[si][2]:snips[si][3]])
			}
		}
		results = append(results, r)
	}
	return results
}

// unwrapDDGHref resolves the //duckduckgo.com/l/?uddg=<encoded>
// redirect wrapper into the real target URL.
func unwrapDDGHref(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	if u, err := url.Parse(href); err == nil {
		if target := u.Query().Get("uddg"); target != "" {
			return target
		}
	}
	return href
}

func cleanText(s string) string {
	s = htmlTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// --- Brave ------------------------------------------------------------

// Brave calls the official JSON API; the subscription key is supplied
// from the control-plane secret SKQUAD_SEARCH_BRAVE_API_KEY.
type Brave struct {
	HTTPClient *http.Client
	APIKey     string
	BaseURL    string // override for tests
}

func NewBrave(client *http.Client, apiKey string) *Brave {
	if client == nil {
		client = http.DefaultClient
	}
	return &Brave{HTTPClient: client, APIKey: apiKey, BaseURL: "https://api.search.brave.com/res/v1/web/search"}
}

func (b *Brave) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if b.APIKey == "" {
		return nil, fmt.Errorf("brave: missing API key (SKQUAD_SEARCH_BRAVE_API_KEY)")
	}
	endpoint := b.BaseURL + "?q=" + url.QueryEscape(query) + fmt.Sprintf("&count=%d", maxResults)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("brave: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.APIKey)
	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("brave: read body: %w", err)
	}
	var payload struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("brave: decode response: %w", err)
	}
	results := make([]Result, 0, min(len(payload.Web.Results), maxResults))
	for _, item := range payload.Web.Results {
		if len(results) >= maxResults {
			break
		}
		results = append(results, Result{
			Title:   cleanText(item.Title),
			URL:     item.URL,
			Snippet: cleanText(item.Description),
		})
	}
	return results, nil
}

// --- Perplexity ---------------------------------------------------------

// Perplexity uses the chat-completions endpoint with the sonar model,
// instructed to answer with a strict JSON array of results. The ADR
// pins only the key source (SKQUAD_SEARCH_PERPLEXITY_API_KEY), so this
// JSON-array protocol is the minimal interpretation (noted in the
// BT-2 report).
type Perplexity struct {
	HTTPClient *http.Client
	APIKey     string
	BaseURL    string // override for tests
	Model      string // override for tests
}

func NewPerplexity(client *http.Client, apiKey string) *Perplexity {
	if client == nil {
		client = http.DefaultClient
	}
	return &Perplexity{
		HTTPClient: client,
		APIKey:     apiKey,
		BaseURL:    "https://api.perplexity.ai/chat/completions",
		Model:      "sonar",
	}
}

func (p *Perplexity) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("perplexity: missing API key (SKQUAD_SEARCH_PERPLEXITY_API_KEY)")
	}
	reqBody := map[string]any{
		"model": p.Model,
		"messages": []map[string]string{
			{
				"role": "system",
				"content": fmt.Sprintf(
					"You are a web search backend. Return ONLY a JSON array of at most %d objects, each with string fields title, url and snippet. No prose, no markdown fences.",
					maxResults,
				),
			},
			{"role": "user", "content": query},
		},
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("perplexity: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("perplexity: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perplexity: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("perplexity: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("perplexity: read body: %w", err)
	}
	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("perplexity: decode response: %w", err)
	}
	if len(payload.Choices) == 0 {
		return nil, fmt.Errorf("perplexity: empty choices in response")
	}
	content := extractJSONArray(payload.Choices[0].Message.Content)
	var items []Result
	if err := json.Unmarshal([]byte(content), &items); err != nil {
		return nil, fmt.Errorf("perplexity: response content is not a JSON array of results: %w", err)
	}
	if len(items) > maxResults {
		items = items[:maxResults]
	}
	return items, nil
}

// extractJSONArray strips markdown fences / surrounding prose so a
// model that wrapped its array in ```json ... ``` still parses.
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
