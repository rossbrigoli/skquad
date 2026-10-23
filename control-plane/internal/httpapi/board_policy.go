package httpapi

import (
	"net/http"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// Squad board policy (S-289, ADR-0016 D8).
//
//	GET /api/v1/squads/{squadID}/board-policy  — raw document + platform-resolved effective values
//	PUT /api/v1/squads/{squadID}/board-policy  — replace the whole document (squad owner only, audited)
//
// Effective-value resolution lives in the domain layer
// (EffectiveSelfApprove / EffectiveReopenCap / EffectiveLeaderApproval)
// so transition guards (S-290/S-294) evaluate it at transition time —
// config changes take effect immediately, with no agent restart.

// boardPolicyResponse carries both the stored document and the effective
// values so the UI can show overrides next to defaults.
type boardPolicyResponse struct {
	Raw       domain.BoardPolicy          `json:"raw"`
	Effective domain.EffectiveBoardPolicy `json:"effective"`
}

func (s *Server) getBoardPolicy(w http.ResponseWriter, r *http.Request) {
	// ADR-0016 D8: the board-policy page is squad-owner-only.
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, boardPolicyResponse{
		Raw:       squad.BoardPolicy,
		Effective: squad.BoardPolicy.Effective(),
	})
}

func (s *Server) putBoardPolicy(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadOwnedSquad(w, r)
	if !ok {
		return
	}

	// PUT replaces the whole policy document: fields omitted from the body
	// become unset overrides (i.e. fall back to platform defaults).
	var policy domain.BoardPolicy
	if !decodeJSON(w, r, &policy) {
		return
	}
	if err := policy.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_board_policy", err.Error())
		return
	}

	ctx := s.pendingUserAuditCtx(r, "squad.board_policy.update", "squad", squad.ID, squad.ID, nil)
	updated, err := s.store.UpdateBoardPolicy(ctx, squad.ID, policy)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boardPolicyResponse{
		Raw:       updated.BoardPolicy,
		Effective: updated.BoardPolicy.Effective(),
	})
}
