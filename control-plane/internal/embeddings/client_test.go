package embeddings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmbedHappyPath(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		gotBody = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float64{0.1, 0.2, 0.3}}},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "master-key-1", "qwen3-embed-0.6b")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	vec, err := c.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vec) != 3 || vec[2] != 0.3 {
		t.Fatalf("vec = %v", vec)
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer master-key-1" {
		t.Fatalf("auth = %q", gotAuth)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body json: %v", err)
	}
	if body["model"] != "qwen3-embed-0.6b" || body["input"] != "hello world" {
		t.Fatalf("body = %v", body)
	}
}

func TestEmbedErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no such model"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "k", "m")
	_, err := c.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("want error on 404")
	}
}

func TestEmbedEmptyDataRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": []}`))
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "k", "m")
	_, err := c.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("want error on empty data")
	}
}

func TestNewClientValidatesURL(t *testing.T) {
	for _, bad := range []string{"", "not a url", "ftp://x", "http://"} {
		if _, err := NewClient(bad, "k", "m"); err == nil {
			t.Fatalf("want error for %q", bad)
		}
	}
	if _, err := NewClient("http://ok.example", "k", ""); err == nil {
		t.Fatal("want error for empty model")
	}
	c, err := NewClient("http://ok.example/", "k", "m")
	if err != nil || c.baseURL != "http://ok.example" {
		t.Fatalf("client = %+v err = %v", c, err)
	}
}

func TestEmbedHonoursContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "k", "m")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Embed(ctx, "x"); err == nil {
		t.Fatal("want deadline error")
	}
}
