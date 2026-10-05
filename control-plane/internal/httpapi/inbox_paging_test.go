package httpapi

// S-239: GET /inbox paging — ?offset= shifts the window and every
// response carries X-Total-Count (the total messages matching the
// filter), while the body stays a bare JSON array for compatibility.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func getInboxRaw(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/inbox"+query, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestInboxListPagingOffsetAndTotalCount(t *testing.T) {
	handler, _, _, agent, credential := agentRuntimeSetup(t, "s239-paging")

	for i := 0; i < 5; i++ {
		doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/inbox",
			map[string]any{"message": fmt.Sprintf("paged %d", i)}, http.StatusCreated, &domain.InboxMessage{})
	}

	// Default (no offset): full first window + total header.
	rec := getInboxRaw(t, handler, "?limit=2")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "5", rec.Header().Get("X-Total-Count"))
	var page []domain.InboxMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page, 2)

	// Second page returns different messages; total is stable.
	rec = getInboxRaw(t, handler, "?limit=2&offset=2")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "5", rec.Header().Get("X-Total-Count"))
	var page2 []domain.InboxMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page2))
	require.Len(t, page2, 2)
	require.NotEqual(t, page[0].ID, page2[0].ID)
	require.NotEqual(t, page[1].ID, page2[0].ID)

	// Last partial page.
	rec = getInboxRaw(t, handler, "?limit=2&offset=4")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "5", rec.Header().Get("X-Total-Count"))
	var page3 []domain.InboxMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page3))
	require.Len(t, page3, 1)

	// Pages are disjoint and cover the whole inbox.
	seen := map[string]bool{}
	for _, q := range []string{"?limit=2", "?limit=2&offset=2", "?limit=2&offset=4"} {
		rec = getInboxRaw(t, handler, q)
		var items []domain.InboxMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
		for _, m := range items {
			require.False(t, seen[m.ID], "message repeated across pages: %s", m.ID)
			seen[m.ID] = true
		}
	}
	require.Len(t, seen, 5)
}

func TestInboxListOffsetValidation(t *testing.T) {
	handler, _, _, _, _ := agentRuntimeSetup(t, "s239-offset-validation")

	rec := getInboxRaw(t, handler, "?offset=-1")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = getInboxRaw(t, handler, "?offset=nope")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Backward compatible: no offset at all still works.
	rec = getInboxRaw(t, handler, "")
	require.Equal(t, http.StatusOK, rec.Code)
}
