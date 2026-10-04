// S-158: prompt templates. Platform admins author reusable starting points
// for squad and agent system prompts; every authenticated user can list
// them for the create-time picker. Selecting a template COPIES its content
// into the draft prompt — templates are never referenced live.
package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/promptcompo"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

type promptTemplateRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Content     *string `json:"content"`
	AppliesTo   *string `json:"applies_to"`
}

// tiersForAppliesTo returns the prompt tiers a template must satisfy.
func tiersForAppliesTo(appliesTo string) []promptcompo.TierName {
	switch appliesTo {
	case domain.PromptTemplateAppliesSquad:
		return []promptcompo.TierName{promptcompo.TierSquad}
	case domain.PromptTemplateAppliesAgent:
		return []promptcompo.TierName{promptcompo.TierAgent}
	default:
		return []promptcompo.TierName{promptcompo.TierSquad, promptcompo.TierAgent}
	}
}

// validateTemplateContent runs the same sanitize/template-var/token-cap
// battery as the editable tiers, for every tier the template applies to.
func validateTemplateContent(content, appliesTo string) *promptFailure {
	for _, tier := range tiersForAppliesTo(appliesTo) {
		if _, _, failure := checkPromptDraft(tier, content); failure != nil {
			return failure
		}
	}
	return nil
}

func (s *Server) listPromptTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := s.store.ListPromptTemplates(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	// Optional picker filter: ?applies_to=agent also matches "both".
	if filter := strings.TrimSpace(r.URL.Query().Get("applies_to")); filter != "" {
		if !domain.ValidPromptTemplateAppliesTo(filter) {
			writeError(w, http.StatusBadRequest, "bad_request", msgAppliesToInvalid)
			return
		}
		filtered := make([]*domain.PromptTemplate, 0, len(templates))
		for _, t := range templates {
			if t.AppliesTo == filter || t.AppliesTo == domain.PromptTemplateAppliesBoth {
				filtered = append(filtered, t)
			}
		}
		templates = filtered
	}
	writeJSON(w, http.StatusOK, templates)
}

func (s *Server) getPromptTemplate(w http.ResponseWriter, r *http.Request) {
	template, err := s.store.GetPromptTemplate(r.Context(), chi.URLParam(r, "templateID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, template)
}

func (s *Server) createPromptTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req promptTemplateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(derefString(req.Name))
	if name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	appliesTo := strings.TrimSpace(derefString(req.AppliesTo))
	if appliesTo == "" {
		appliesTo = domain.PromptTemplateAppliesAgent
	}
	if !domain.ValidPromptTemplateAppliesTo(appliesTo) {
		writeError(w, http.StatusBadRequest, "bad_request", msgAppliesToInvalid)
		return
	}
	content := strings.TrimSpace(derefString(req.Content))
	if content == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "content is required")
		return
	}
	if failure := validateTemplateContent(content, appliesTo); failure != nil {
		writePromptFailure(w, failure)
		return
	}
	template := &domain.PromptTemplate{
		Name:        name,
		Description: strings.TrimSpace(derefString(req.Description)),
		Content:     content,
		AppliesTo:   appliesTo,
		CreatedBy:   currentUser(r.Context()).ID,
	}
	created, err := s.store.CreatePromptTemplate(r.Context(), template)
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			writeError(w, http.StatusConflict, "name_taken", "a prompt template with that name already exists")
			return
		}
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) updatePromptTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	template, err := s.store.GetPromptTemplate(r.Context(), chi.URLParam(r, "templateID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	var req promptTemplateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "name must not be empty")
			return
		}
		template.Name = name
	}
	if req.Description != nil {
		template.Description = strings.TrimSpace(*req.Description)
	}
	if req.AppliesTo != nil {
		appliesTo := strings.TrimSpace(*req.AppliesTo)
		if !domain.ValidPromptTemplateAppliesTo(appliesTo) {
			writeError(w, http.StatusBadRequest, "bad_request", msgAppliesToInvalid)
			return
		}
		template.AppliesTo = appliesTo
	}
	if req.Content != nil {
		content := strings.TrimSpace(*req.Content)
		if content == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "content must not be empty")
			return
		}
		if failure := validateTemplateContent(content, template.AppliesTo); failure != nil {
			writePromptFailure(w, failure)
			return
		}
		template.Content = content
	}
	updated, err := s.store.UpdatePromptTemplate(r.Context(), template)
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			writeError(w, http.StatusConflict, "name_taken", "a prompt template with that name already exists")
			return
		}
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deletePromptTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	if err := s.store.DeletePromptTemplate(r.Context(), chi.URLParam(r, "templateID")); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// promptTemplateBulkDeleteRequest (S-214) carries the template ids to
// remove in one transactional batch.
type promptTemplateBulkDeleteRequest struct {
	IDs []string `json:"ids"`
}

// bulkDeletePromptTemplates deletes multiple templates at once, gated
// by the same platform_admin check as the single-delete route. Returns
// the count of rows actually deleted (unknown ids are skipped).
func (s *Server) bulkDeletePromptTemplates(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req promptTemplateBulkDeleteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ids := make([]string, 0, len(req.IDs))
	seen := make(map[string]bool, len(req.IDs))
	for _, raw := range req.IDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "ids must not contain blank values")
			return
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "ids is required")
		return
	}
	if len(ids) > 200 {
		writeError(w, http.StatusBadRequest, "bad_request", "at most 200 templates per bulk delete")
		return
	}
	deleted, err := s.store.BulkDeletePromptTemplates(r.Context(), ids)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
