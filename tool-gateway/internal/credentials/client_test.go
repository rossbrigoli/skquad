// TG-4 (S-250): gateway-side credentials client tests. Fake token
// values only.
package credentials

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResolveHappyPath(t *testing.T) {
	var gotResource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/v1/credentials", r.URL.Path)
		gotResource = r.URL.Query().Get("resource")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resource_id":"res-1","kind":"bearer","fields":{"token":"test-token-DO-NOT-USE"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	s, err := c.Resolve(context.Background(), "res-1", "")
	require.NoError(t, err)
	require.Equal(t, "res-1", s.ResourceID)
	require.Equal(t, "bearer", s.Kind)
	require.Equal(t, "test-token-DO-NOT-USE", s.Fields["token"])
	require.Equal(t, "res-1", gotResource, "resource query parameter must be forwarded")
}

func TestResolveNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	_, err := c.Resolve(context.Background(), "missing", "")
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestResolveServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	_, err := c.Resolve(context.Background(), "res-1", "")
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestResolveMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	_, err := c.Resolve(context.Background(), "res-1", "")
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestResolveMissingKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"resource_id":"res-1","fields":{}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	_, err := c.Resolve(context.Background(), "res-1", "")
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestResolveUnreachableFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening

	c := NewClient(url, 500*time.Millisecond)
	_, err := c.Resolve(context.Background(), "res-1", "")
	require.ErrorIs(t, err, ErrUnavailable)
	require.True(t, errors.Is(err, ErrUnavailable))
}

func TestResolveErrorsNeverCarrySecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"denied for token test-token-DO-NOT-USE"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	_, err := c.Resolve(context.Background(), "res-1", "")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "test-token-DO-NOT-USE")
}

// TG-4c (S-259): the agent parameter is forwarded when present and
// omitted when empty (resource-only backwards compat).
func TestResolveForwardsAgentParameter(t *testing.T) {
	var gotAgent string
	var hadAgent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.URL.Query().Get("agent")
		_, hadAgent = r.URL.Query()["agent"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resource_id":"res-1","kind":"bearer","fields":{"token":"agentA-token-DO-NOT-USE"},"scope":"agent"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	s, err := c.Resolve(context.Background(), "res-1", "agent-42")
	require.NoError(t, err)
	require.Equal(t, "agent-42", gotAgent, "agent query parameter must be forwarded")
	require.True(t, hadAgent)
	require.Equal(t, "agentA-token-DO-NOT-USE", s.Fields["token"])

	// Empty agent → parameter omitted entirely.
	hadAgent = false
	_, err = c.Resolve(context.Background(), "res-1", "")
	require.NoError(t, err)
	require.False(t, hadAgent, "empty agent must omit the parameter (backwards compat)")
}
