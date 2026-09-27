package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseDuckDuckGoHTML(t *testing.T) {
	t.Parallel()
	body := `
	<div class="result results_links">
	  <h2 class="result__title">
	    <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&amp;rut=abc">Go Programming Language</a>
	  </h2>
	  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev">Go is an <b>open source</b> programming language.</a>
	</div>
	<div class="result results_links">
	  <h2 class="result__title">
	    <a rel="nofollow" class="result__a" href="https://rust-lang.org/">Rust Programming Language</a>
	  </h2>
	  <a class="result__snippet" href="https://rust-lang.org/">A language empowering everyone &amp; everyone.</a>
	</div>
	<div class="result results_links">
	  <a class="result__a" href="https://ziglang.org/">Zig</a>
	</div>`

	results := ParseDuckDuckGoHTML(body, 10)
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3: %+v", len(results), results)
	}
	if results[0].URL != "https://go.dev/" {
		t.Fatalf("first url = %q, want https://go.dev/ (uddg unwrapped)", results[0].URL)
	}
	if results[0].Title != "Go Programming Language" {
		t.Fatalf("first title = %q", results[0].Title)
	}
	if results[0].Snippet != "Go is an open source programming language." {
		t.Fatalf("first snippet = %q", results[0].Snippet)
	}
	if results[1].URL != "https://rust-lang.org/" || results[1].Snippet != "A language empowering everyone & everyone." {
		t.Fatalf("second result = %+v", results[1])
	}
	if results[2].Snippet != "" {
		t.Fatalf("third snippet = %q, want empty", results[2].Snippet)
	}

	limited := ParseDuckDuckGoHTML(body, 1)
	if len(limited) != 1 {
		t.Fatalf("limited results = %d, want 1", len(limited))
	}
}

func TestDuckDuckGoProviderAgainstStubServer(t *testing.T) {
	t.Parallel()
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		w.Write([]byte(`<a class="result__a" href="https://example.com/a">Alpha</a><a class="result__snippet" href="#">First snippet</a>`))
	}))
	defer srv.Close()

	p := NewDuckDuckGo(srv.Client())
	p.BaseURL = srv.URL + "/html/"
	results, err := p.Search(context.Background(), "skquad test", 5)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/html/" || gotQuery != "skquad test" {
		t.Fatalf("request path=%q q=%q", gotPath, gotQuery)
	}
	if len(results) != 1 || results[0].Title != "Alpha" || results[0].Snippet != "First snippet" {
		t.Fatalf("results = %+v", results)
	}
}

func TestDuckDuckGoProviderNon200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	p := NewDuckDuckGo(srv.Client())
	p.BaseURL = srv.URL + "/html/"
	if _, err := p.Search(context.Background(), "q", 5); err == nil {
		t.Fatal("expected error on non-200")
	}
}

func TestBraveProvider(t *testing.T) {
	t.Parallel()
	var gotToken, gotCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Subscription-Token")
		gotCount = r.URL.Query().Get("count")
		resp := map[string]any{
			"web": map[string]any{
				"results": []map[string]string{
					{"title": "Brave One", "url": "https://one.example", "description": "desc one"},
					{"title": "Brave Two", "url": "https://two.example", "description": "desc two"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewBrave(srv.Client(), "secret-key")
	p.BaseURL = srv.URL + "/res/v1/web/search"
	results, err := p.Search(context.Background(), "brave query", 1)
	if err != nil {
		t.Fatal(err)
	}
	if gotToken != "secret-key" || gotCount != "1" {
		t.Fatalf("token=%q count=%q", gotToken, gotCount)
	}
	if len(results) != 1 || results[0].Title != "Brave One" || results[0].URL != "https://one.example" || results[0].Snippet != "desc one" {
		t.Fatalf("results = %+v", results)
	}
}

func TestBraveMissingKey(t *testing.T) {
	t.Parallel()
	p := NewBrave(nil, "")
	_, err := p.Search(context.Background(), "q", 5)
	if err == nil || err.Error() != "brave: missing API key (SKQUAD_SEARCH_BRAVE_API_KEY)" {
		t.Fatalf("err = %v, want missing-key error naming the env var", err)
	}
}

func TestPerplexityProvider(t *testing.T) {
	t.Parallel()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{
					"content": "```json\n[{\"title\":\"P1\",\"url\":\"https://p1.example\",\"snippet\":\"s1\"},{\"title\":\"P2\",\"url\":\"https://p2.example\",\"snippet\":\"s2\"}]\n```",
				}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewPerplexity(srv.Client(), "ppx-key")
	p.BaseURL = srv.URL + "/chat/completions"
	results, err := p.Search(context.Background(), "ppx query", 1)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ppx-key" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if len(results) != 1 || results[0].Title != "P1" || results[0].Snippet != "s1" {
		t.Fatalf("results = %+v", results)
	}
}

func TestPerplexityNonJSONContent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "I could not search today."}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	p := NewPerplexity(srv.Client(), "k")
	p.BaseURL = srv.URL + "/chat/completions"
	if _, err := p.Search(context.Background(), "q", 3); err == nil {
		t.Fatal("expected decode error for non-JSON content")
	}
}

func TestPerplexityMissingKey(t *testing.T) {
	t.Parallel()
	p := NewPerplexity(nil, "")
	_, err := p.Search(context.Background(), "q", 5)
	if err == nil || err.Error() != "perplexity: missing API key (SKQUAD_SEARCH_PERPLEXITY_API_KEY)" {
		t.Fatalf("err = %v, want missing-key error naming the env var", err)
	}
}
