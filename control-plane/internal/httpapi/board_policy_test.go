package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// S-289 (ADR-0016 D8): GET/PUT /squads/{id}/board-policy.

type boardPolicyResp struct {
	Raw       domain.BoardPolicy          `json:"raw"`
	Effective domain.EffectiveBoardPolicy `json:"effective"`
}

func TestBoardPolicyGetDefaults(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "Policy Squad"}, http.StatusCreated, &squad)

	var resp boardPolicyResp
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/board-policy", nil, http.StatusOK, &resp)
	require.Equal(t, 3, resp.Effective.ReopenCapPerDay)
	require.True(t, resp.Effective.SelfApproveDefault)
	require.False(t, resp.Effective.LeaderApprovalAllowed)
	require.Nil(t, resp.Raw.ReopenCapPerDay)
	require.Nil(t, resp.Raw.SelfApproveDefault)
	require.Nil(t, resp.Raw.LeaderApprovalAllowed)
}

func TestBoardPolicyPutGetRoundTrip(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "Policy Squad"}, http.StatusCreated, &squad)
	path := "/api/v1/squads/" + squad.ID + "/board-policy"

	var put boardPolicyResp
	doJSON(t, handler, http.MethodPut, path, map[string]any{
		"reopen_cap_per_day":      5,
		"self_approve_default":    false,
		"leader_approval_allowed": true,
	}, http.StatusOK, &put)
	require.Equal(t, 5, put.Effective.ReopenCapPerDay)
	require.False(t, put.Effective.SelfApproveDefault)
	require.True(t, put.Effective.LeaderApprovalAllowed)
	require.NotNil(t, put.Raw.ReopenCapPerDay)
	require.Equal(t, 5, *put.Raw.ReopenCapPerDay)

	var got boardPolicyResp
	doJSON(t, handler, http.MethodGet, path, nil, http.StatusOK, &got)
	require.Equal(t, put.Effective, got.Effective)
	require.Equal(t, put.Raw.ReopenCapPerDay, got.Raw.ReopenCapPerDay)

	// The mutation is audited (owner-only mutation audit pattern, S-86).
	entries, err := store.ListAudit(context.Background(), squad.ID, 100)
	require.NoError(t, err)
	var found bool
	for _, e := range entries {
		if e.Action == "squad.board_policy.update" && e.ActorType == "user" && e.ResourceType == "squad" && e.ResourceID == squad.ID {
			found = true
		}
	}
	require.True(t, found, "expected squad.board_policy.update audit entry")

	// Whole-document replacement: a partial PUT clears the other overrides
	// back to platform defaults.
	var replaced boardPolicyResp
	doJSON(t, handler, http.MethodPut, path, map[string]any{"reopen_cap_per_day": 1}, http.StatusOK, &replaced)
	require.Equal(t, 1, replaced.Effective.ReopenCapPerDay)
	require.True(t, replaced.Effective.SelfApproveDefault, "cleared override falls back to platform true")
	require.False(t, replaced.Effective.LeaderApprovalAllowed)
	require.Nil(t, replaced.Raw.SelfApproveDefault)
	require.Nil(t, replaced.Raw.LeaderApprovalAllowed)
}

func TestBoardPolicyPutValidation(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "Policy Squad"}, http.StatusCreated, &squad)
	path := "/api/v1/squads/" + squad.ID + "/board-policy"

	// Negative cap -> 400 invalid_board_policy.
	doJSONNoBody(t, handler, http.MethodPut, path, map[string]any{"reopen_cap_per_day": -1}, http.StatusBadRequest)
	var errBody map[string]map[string]string
	doJSONAuth(t, handler, "", http.MethodPut, path, map[string]any{"reopen_cap_per_day": -7}, http.StatusBadRequest, &errBody)
	require.Equal(t, "invalid_board_policy", errBody["error"]["code"])

	// Cap 0 is valid (reopens disabled for agents).
	var zero boardPolicyResp
	doJSON(t, handler, http.MethodPut, path, map[string]any{"reopen_cap_per_day": 0}, http.StatusOK, &zero)
	require.Equal(t, 0, zero.Effective.ReopenCapPerDay)
}

func TestBoardPolicyOwnerOnly(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authOwner:  {Email: "owner@example.com", Name: "Owner"},
		authViewer: {Email: "viewer@example.com", Name: "Viewer"},
	})

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{"name": "Owned Squad"}, http.StatusCreated, &squad)
	path := "/api/v1/squads/" + squad.ID + "/board-policy"

	// Non-owner user: GET and PUT are both forbidden.
	doJSONAuth(t, handler, authViewer, http.MethodGet, path, nil, http.StatusForbidden, &map[string]any{})
	doJSONAuth(t, handler, authViewer, http.MethodPut, path, map[string]any{"reopen_cap_per_day": 9}, http.StatusForbidden, &map[string]any{})

	// Owner still works.
	var resp boardPolicyResp
	doJSONAuth(t, handler, authOwner, http.MethodPut, path, map[string]any{"reopen_cap_per_day": 9}, http.StatusOK, &resp)
	require.Equal(t, 9, resp.Effective.ReopenCapPerDay)
}
