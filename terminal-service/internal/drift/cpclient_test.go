package drift

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCPClientListArtifactResources(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/artifact-resources" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": []ArtifactResource{baseResource()}})
	}))
	defer srv.Close()

	c := &CPClient{BaseURL: srv.URL, Token: "tok-123"}
	got, err := c.ListArtifactResources(context.Background())
	if err != nil {
		t.Fatalf("ListArtifactResources: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if len(got) != 1 || got[0].ResourceID != "res-1" || len(got[0].HostGroups) != 2 {
		t.Fatalf("decoded = %+v", got)
	}
}

func TestCPClientListErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthorized"}}`)
	}))
	defer srv.Close()
	c := &CPClient{BaseURL: srv.URL, Token: "bad"}
	if _, err := c.ListArtifactResources(context.Background()); err == nil {
		t.Fatalf("non-200 must error")
	}
}

func TestCPClientPostDriftReport(t *testing.T) {
	var gotAuth, gotCT string
	var body Report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "dr-42", "in_sync": body.InSync})
	}))
	defer srv.Close()

	c := &CPClient{BaseURL: srv.URL + "/", Token: "tok-9"}
	id, err := c.PostDriftReport(context.Background(), NewReport("res-1", "web", "site.yml", tipSHA, []string{"host2"}))
	if err != nil {
		t.Fatalf("PostDriftReport: %v", err)
	}
	if id != "dr-42" {
		t.Fatalf("id = %q", id)
	}
	if gotAuth != "Bearer tok-9" || gotCT != "application/json" {
		t.Fatalf("headers auth=%q ct=%q", gotAuth, gotCT)
	}
	if body.ResourceID != "res-1" || body.HostGroup != "web" || body.Playbook != "site.yml" || body.GitRev != tipSHA {
		t.Fatalf("payload = %+v", body)
	}
	if len(body.DriftedHosts) != 1 || body.DriftedHosts[0] != "host2" || body.InSync {
		t.Fatalf("drift fields = %+v", body)
	}
}

func TestCPClientPostErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"bad_request"}}`)
	}))
	defer srv.Close()
	c := &CPClient{BaseURL: srv.URL, Token: "tok"}
	if _, err := c.PostDriftReport(context.Background(), NewReport("r", "g", "p", tipSHA, nil)); err == nil {
		t.Fatalf("non-200 must error")
	}
}
