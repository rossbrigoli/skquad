// S-194: image uploads for the chat composer and task threads.
//
// Design notes:
//   - Uploads are squad-scoped at creation (?squad_id=...) and every read
//     re-checks squad access, so there is no public unauthenticated path
//     and no "unguessable URL only" exposure.
//   - Content type is validated by sniffing the bytes (magic numbers),
//     never by the client-supplied extension or Content-Type header.
//   - Bytes live in Postgres (bytea) for now; the long-term home is
//     object storage with this table metadata-only. See the S-194 report.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// Attachment validation errors surfaced by resolveUploadAttachments.
var (
	errTooManyAttachments  = errors.New("too many attachments")
	errInvalidAttachment   = errors.New("invalid attachment")
)

// maxUploadBytes bounds a single image attachment (5 MiB). The request
// body limit is enforced with http.MaxBytesReader so an oversized upload
// cannot exhaust memory before validation.
const maxUploadBytes int64 = 5 << 20

// uploadFormField is the multipart form field carrying the image.
const uploadFormField = "file"

// sniffedImageTypes maps the canonical MIME types we accept for image
// attachments. Keys are what http.DetectContentType returns for the
// respective magic bytes.
var sniffedImageTypes = map[string]string{
	"image/png":  "image/png",
	"image/jpeg": "image/jpeg",
	"image/gif":  "image/gif",
}

// detectImageContentType sniffs the leading bytes of an image and returns
// the canonical MIME type. WebP needs a custom check because
// http.DetectContentType does not know it ("RIFF....WEBP").
// Returns an empty string when the bytes are not an accepted image.
func detectImageContentType(sniff []byte) string {
	if len(sniff) >= 12 && string(sniff[0:4]) == "RIFF" && string(sniff[8:12]) == "WEBP" {
		return "image/webp"
	}
	if detected, ok := sniffedImageTypes[http.DetectContentType(sniff)]; ok {
		return detected
	}
	return ""
}

// uploadResponse is the JSON shape returned after a successful upload and
// embedded (normalized) into message payloads as one entry of
// `payload.attachments`.
type uploadResponse struct {
	ID          string `json:"id"`
	SquadID     string `json:"squad_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	URL         string `json:"url"`
}

func uploadToResponse(u *domain.Upload) uploadResponse {
	return uploadResponse{
		ID:          u.ID,
		SquadID:     u.SquadID,
		Filename:    u.Filename,
		ContentType: u.ContentType,
		SizeBytes:   u.SizeBytes,
		URL:         "/api/v1/uploads/" + u.ID,
	}
}

// sanitizeUploadFilename keeps the caller's label but strips path
// separators and control characters so it is safe for display and for
// the Content-Disposition header.
func sanitizeUploadFilename(raw string) string {
	name := strings.TrimSpace(raw)
	if idx := strings.LastIndexAny(name, "/\\"); idx >= 0 {
		name = name[idx+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.TrimSpace(b.String())
	if name == "" {
		name = "image"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// createUpload handles POST /api/v1/uploads?squad_id=<id> with a
// multipart/form-data body containing the "file" field. Returns 201 with
// the upload metadata + URL.
func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	squadID := strings.TrimSpace(r.URL.Query().Get("squad_id"))
	if squadID == "" {
		writeError(w, http.StatusBadRequest, "missing_squad", "squad_id query parameter is required")
		return
	}
	if _, ok := s.ensureSquadAccess(w, r, squadID, false); !ok {
		return
	}

	// Hard-cap the body before any parsing so oversized uploads die at
	// the reader, not in the multipart buffer.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+64*1024)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "upload_too_large", "image exceeds the 5 MB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_upload", "multipart/form-data upload with a \"file\" field is required")
		return
	}
	file, header, err := r.FormFile(uploadFormField)
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing_file", "multipart field \"file\" is required")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_upload", "could not read uploaded file")
		return
	}
	if int64(len(data)) > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "upload_too_large", "image exceeds the 5 MB limit")
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "empty_file", "uploaded file is empty")
		return
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	contentType := detectImageContentType(sniff)
	if contentType == "" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "only png, jpeg, gif and webp images are accepted")
		return
	}

	u := currentUser(r.Context())
	created, err := s.store.CreateUpload(s.pendingUserAuditCtx(r, "upload.create", "upload", "", squadID, nil), &domain.Upload{
		SquadID:     squadID,
		UploaderID:  u.ID,
		Filename:    sanitizeUploadFilename(header.Filename),
		ContentType: contentType,
		SizeBytes:   int64(len(data)),
		Data:        data,
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, uploadToResponse(created))
}

// isTooLarge reports whether a ParseMultipartForm error was caused by
// the MaxBytesReader cap (as opposed to malformed multipart).
func isTooLarge(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "message too large") || strings.Contains(msg, "http: request body too large")
}

// serveUpload writes the raw image bytes with the stored content type.
// Caching is private: the bytes are access-controlled, never public.
func serveUpload(w http.ResponseWriter, u *domain.Upload) {
	w.Header().Set("Content-Type", u.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(u.SizeBytes, 10))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Disposition", `inline; filename="`+strings.ReplaceAll(u.Filename, `"`, "")+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(u.Data)
}

// getUpload serves GET /api/v1/uploads/{uploadID} to humans: the caller
// must have read access to the upload's squad.
func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetUpload(r.Context(), chi.URLParam(r, "uploadID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if _, ok := s.ensureSquadAccess(w, r, u.SquadID, false); !ok {
		return
	}
	serveUpload(w, u)
}

// getMyUploadBytes serves GET /api/v1/agents/me/uploads/{uploadID} to
// the agent runtime: an agent may fetch images attached within its own
// squad (the S-194 payload URLs are retrievable by the receiving agent
// with its own credential). Vision passthrough itself is a follow-up —
// today the runtime only sees the URL text.
func (s *Server) getMyUploadBytes(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	u, err := s.store.GetUpload(r.Context(), chi.URLParam(r, "uploadID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if u.SquadID != principal.Agent.SquadID {
		writeError(w, http.StatusForbidden, "forbidden", "upload belongs to another squad")
		return
	}
	serveUpload(w, u)
}

// resolveUploadAttachments validates the attachment upload IDs on an
// outgoing message and returns the normalized `attachments` payload
// entries. Every ID must be a well-formed upload that exists and belongs
// to the message's squad — no cross-squad references, no forged URLs.
// Returns (nil, nil) when the list is empty.
func (s *Server) resolveUploadAttachments(r *http.Request, squadID string, ids []string) ([]uploadResponse, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 8 {
		return nil, errTooManyAttachments
	}
	out := make([]uploadResponse, 0, len(ids))
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if _, err := uuid.Parse(id); err != nil {
			return nil, errInvalidAttachment
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		u, err := s.store.GetUpload(r.Context(), id)
		if err != nil {
			return nil, errInvalidAttachment
		}
		if u.SquadID != squadID {
			return nil, errInvalidAttachment
		}
		out = append(out, uploadToResponse(u))
	}
	return out, nil
}

// withAttachments merges the normalized attachment list into a message
// payload (S-194). Empty lists leave the payload untouched.
func withAttachments(payload json.RawMessage, attachments []uploadResponse) json.RawMessage {
	if len(attachments) == 0 {
		return payload
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		obj = map[string]any{}
	}
	obj["attachments"] = attachments
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// attachmentError maps attachment validation failures to HTTP errors.
func (s *Server) writeAttachmentError(w http.ResponseWriter, err error) {
	switch {
	case err == errTooManyAttachments:
		writeError(w, http.StatusBadRequest, "too_many_attachments", "a message may carry at most 8 attachments")
	default:
		writeError(w, http.StatusBadRequest, "invalid_attachment", "attachment ids must reference images uploaded to this squad")
	}
}
