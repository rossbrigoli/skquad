package httpapi

// S-216: inbox attachments.
//
// Agents deliver files alongside send_inbox messages. The agent-facing
// upload path is the SAME endpoint and auth as the JSON send_inbox:
// when the request is multipart/form-data, the message fields arrive
// as form fields and the files as repeated "attachments" parts. The
// control plane validates every file server-side (domain.
// InspectInboxAttachment: extension denylist, executable magic-byte
// rejection, inspectability allowlist), computes sha256, and stores it
// against the created message. A rejected file fails the whole send
// with a clear per-file reason — nothing half-delivered.
//
// Deviation from the original design note ("new upload endpoint, then
// reference by id"): a single atomic multipart POST avoids a staging
// area for orphaned uploads and keeps message+attachments consistent
// in one request. The download route is separate (below).
//
// Download authorization: only the message's recipient (or a platform
// admin) may fetch attachment bytes — the same rule as the message
// itself. Bytes are served with the stored sniffed content type,
// nosniff, and Content-Disposition: attachment (inline only for a
// small safe-image set so the Inbox can render previews).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// maxInboxMultipartBody bounds a whole send_inbox multipart request:
// the per-message attachment budget plus headroom for the text fields.
const maxInboxMultipartBody = domain.MaxInboxAttachmentsPerMsg*domain.MaxInboxAttachmentBytes + (1 << 20)

// inboxAttachmentURL builds the authenticated download URL for one
// attachment of one message.
func inboxAttachmentURL(messageID, attachmentID string) string {
	return "/api/v1/inbox/" + messageID + "/attachments/" + attachmentID
}

// inboxAttachmentMeta renders the JSON-safe metadata for one stored
// attachment (never the bytes).
func inboxAttachmentMeta(a *domain.InboxAttachment) map[string]any {
	return map[string]any{
		"id":           a.ID,
		"message_id":   a.MessageID,
		"squad_id":     a.SquadID,
		"filename":     a.Filename,
		"content_type": a.ContentType,
		"size_bytes":   a.SizeBytes,
		"sha256":       a.SHA256,
		"url":          inboxAttachmentURL(a.MessageID, a.ID),
		"created_at":   a.CreatedAt,
	}
}

// sendInboxFromAgent is the send_inbox builtin's endpoint: an agent
// delivers human-requested content (and optionally files) to its squad
// owner's inbox. JSON bodies carry message/subject/task_id only;
// multipart/form-data bodies may additionally carry up to
// MaxInboxAttachmentsPerMsg files under the "attachments" field.
func (s *Server) sendInboxFromAgent(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	squad, err := s.store.GetSquad(r.Context(), principal.Agent.SquadID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if squad.OwnerID == "" {
		writeError(w, http.StatusNotFound, "not_found", "squad owner not found for notification")
		return
	}

	var req struct {
		Message string `json:"message"`
		Subject string `json:"subject"`
		TaskID  string `json:"task_id"`
	}
	var files []pendingInboxFile

	if isMultipart(r) {
		var ok bool
		files, ok = parseInboxMultipart(w, r, &req)
		if !ok {
			return
		}
	} else if !decodeJSON(w, r, &req) {
		return
	}

	message := trimRunes(strings.TrimSpace(req.Message), maxInboxMessageChars)
	if message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	taskID := strings.TrimSpace(req.TaskID)
	if taskID != "" {
		task, err := s.store.GetTask(r.Context(), taskID)
		if err != nil || task.SquadID != principal.Agent.SquadID {
			writeError(w, http.StatusNotFound, "not_found", "task not found in your squad")
			return
		}
	}

	// Validate every attachment BEFORE creating the message so a bad
	// file never leaves a message with partial attachments behind.
	validated, ok := validateInboxFiles(w, files)
	if !ok {
		return
	}

	subject := trimRunes(strings.TrimSpace(req.Subject), 200)
	created, err := s.store.CreateInboxMessage(s.pendingAgentAuditCtx(r, principal.Agent.ID, "inbox.send_inbox", "inbox_message", "", principal.Agent.SquadID, nil), &domain.InboxMessage{
		SquadID:     principal.Agent.SquadID,
		UserID:      squad.OwnerID,
		FromAgentID: principal.Agent.ID,
		TaskID:      taskID,
		Kind:        domain.InboxAgentMessage,
		Message:     message,
		Subject:     subject,
		Body:        message,
	})
	if err != nil {
		if err == storage.ErrNotFound {
			writeError(w, http.StatusNotFound, "not_found", "squad owner not found for notification")
			return
		}
		writeStorageError(w, err)
		return
	}

	// Persist attachments against the message. A storage failure here
	// is reported loudly (the message exists; the agent can retry the
	// whole send — duplicate messages are preferable to silent loss).
	if !s.persistInboxAttachments(w, r, created, validated) {
		return
	}
	s.attachAttachmentURLs(created)
	writeJSON(w, http.StatusCreated, created)
}

// pendingInboxFile is an uploaded-but-unvalidated attachment (S-189:
// hoisted out of sendInboxFromAgent for the extracted helpers).
type pendingInboxFile struct {
	filename string
	data     []byte
}

// validatedInboxFile is an attachment that passed domain inspection.
type validatedInboxFile struct {
	filename    string
	contentType string
	data        []byte
}

// parseInboxMultipart reads the multipart form fields and attachment
// parts for send_inbox (S-189 split). Returns the pending files; ok is
// false when an error response was written.
func parseInboxMultipart(w http.ResponseWriter, r *http.Request, req *struct {
	Message string `json:"message"`
	Subject string `json:"subject"`
	TaskID  string `json:"task_id"`
}) ([]pendingInboxFile, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxInboxMultipartBody)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "attachments_too_large",
				fmt.Sprintf("request exceeds the %d MB total attachment budget", maxInboxMultipartBody>>20))
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "bad_upload", "malformed multipart/form-data body")
		return nil, false
	}
	req.Message = r.FormValue("message")
	req.Subject = r.FormValue("subject")
	req.TaskID = r.FormValue("task_id")
	var files []pendingInboxFile
	parts := r.MultipartForm.File["attachments"]
	if len(parts) > domain.MaxInboxAttachmentsPerMsg {
		writeError(w, http.StatusBadRequest, "too_many_attachments",
			fmt.Sprintf("a message may carry at most %d attachments", domain.MaxInboxAttachmentsPerMsg))
		return nil, false
	}
	for _, part := range parts {
		f, err := part.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_upload", "could not read attachment "+part.Filename)
			return nil, false
		}
		data, err := io.ReadAll(io.LimitReader(f, domain.MaxInboxAttachmentBytes+1))
		f.Close()
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_upload", "could not read attachment "+part.Filename)
			return nil, false
		}
		files = append(files, pendingInboxFile{filename: part.Filename, data: data})
	}
	return files, true
}

// validateInboxFiles runs domain inspection over every pending file
// before the message row exists (S-189 split).
func validateInboxFiles(w http.ResponseWriter, files []pendingInboxFile) ([]validatedInboxFile, bool) {
	validated := make([]validatedInboxFile, 0, len(files))
	for _, f := range files {
		contentType, rejection := domain.InspectInboxAttachment(f.filename, f.data)
		if rejection != "" {
			writeError(w, http.StatusBadRequest, "attachment_rejected",
				fmt.Sprintf("attachment %q rejected: %s", f.filename, rejection))
			return nil, false
		}
		validated = append(validated, validatedInboxFile{
			filename:    sanitizeUploadFilename(f.filename),
			contentType: contentType,
			data:        f.data,
		})
	}
	return validated, true
}

// persistInboxAttachments stores each validated attachment against the
// created message (S-189 split).
func (s *Server) persistInboxAttachments(w http.ResponseWriter, r *http.Request, created *domain.InboxMessage, validated []validatedInboxFile) bool {
	for _, v := range validated {
		sum := sha256.Sum256(v.data)
		stored, err := s.store.CreateInboxAttachment(r.Context(), &domain.InboxAttachment{
			MessageID:   created.ID,
			SquadID:     created.SquadID,
			Filename:    v.filename,
			ContentType: v.contentType,
			SizeBytes:   int64(len(v.data)),
			SHA256:      hex.EncodeToString(sum[:]),
			Data:        v.data,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "attachment_store_failed",
				fmt.Sprintf("message %s created but attachment %q could not be stored: %v", created.ID, v.filename, err))
			return false
		}
		created.Attachments = append(created.Attachments, *stored)
	}
	return true
}

// isMultipart reports whether the request carries a multipart body.
func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data")
}

// attachAttachmentURLs fills the computed download URL on the
// attachment metadata of an inbox message response.
func (s *Server) attachAttachmentURLs(msg *domain.InboxMessage) {
	for i := range msg.Attachments {
		msg.Attachments[i].URL = inboxAttachmentURL(msg.ID, msg.Attachments[i].ID)
	}
}

// enrichInboxAttachments batch-loads attachment metadata for the
// listed messages (no bytes) so the UI can render chips without an
// N+1 per-message query.
func (s *Server) enrichInboxAttachments(r *http.Request, messages []*domain.InboxMessage) {
	if len(messages) == 0 {
		return
	}
	ids := make([]string, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	meta, err := s.store.ListInboxAttachmentMeta(r.Context(), ids)
	if err != nil {
		// Attachment metadata is additive; a lookup failure must not
		// break the inbox listing itself.
		return
	}
	for _, m := range messages {
		if atts, ok := meta[m.ID]; ok {
			m.Attachments = atts
			s.attachAttachmentURLs(m)
		}
	}
}

// serveInboxAttachment streams one attachment's bytes. Authorization:
// the caller must be the message's recipient or a platform admin —
// the same rule that governs reading the message itself.
func (s *Server) serveInboxAttachment(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	messageID := chi.URLParam(r, "messageID")
	attachmentID := chi.URLParam(r, "attachmentID")

	msg, err := s.store.GetInboxMessage(r.Context(), messageID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if msg.UserID != u.ID {
		if !s.requirePlatformAdmin(w, r) {
			return
		}
	}
	att, err := s.store.GetInboxAttachment(r.Context(), attachmentID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if att.MessageID != messageID {
		// Attachment does not belong to the requested message — treat
		// as not found, never serve cross-message bytes.
		writeError(w, http.StatusNotFound, "not_found", "attachment not found on this message")
		return
	}

	disposition := "attachment"
	if safeInlineImage(att.ContentType) {
		disposition = "inline" // Inbox image previews
	}
	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(att.SizeBytes, 10))
	w.Header().Set("Content-Disposition", disposition+`; filename="`+strings.ReplaceAll(att.Filename, `"`, "")+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(att.Data)
}

// safeInlineImage reports whether a content type is safe to render
// inline in the browser (preview). SVG and everything else is forced to
// download: SVG can carry scripts, and only raster formats are
// previewable here.
func safeInlineImage(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return true
	default:
		return false
	}
}

// inboxAttachmentMetaHandler serves GET /inbox/{messageID}/attachments
// metadata for one message (recipient/admin only) — used by the UI to
// refresh attachment lists without re-listing the inbox.
func (s *Server) listInboxMessageAttachments(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	messageID := chi.URLParam(r, "messageID")
	msg, err := s.store.GetInboxMessage(r.Context(), messageID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if msg.UserID != u.ID {
		if !s.requirePlatformAdmin(w, r) {
			return
		}
	}
	meta, err := s.store.ListInboxAttachmentMeta(r.Context(), []string{messageID})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	atts := meta[messageID]
	s.attachAttachmentURLs(msg)
	out := make([]map[string]any, 0, len(atts))
	for i := range atts {
		out = append(out, inboxAttachmentMeta(&atts[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": out})
}
