// S-194: image attachment tests — upload, serve, size/type limits, and
// attachment binding on chat + task messages.
package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// Minimal magic-byte-valid images (content sniffing only needs the header).
var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x01}, 64)...)
	gifBytes  = append([]byte("GIF89a"), bytes.Repeat([]byte{0x02}, 64)...)
	jpegBytes = append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{0x03}, 64)...)
	webpBytes = append(append([]byte("RIFF"), 0xf0, 0x00, 0x00, 0x00), append([]byte("WEBP"), bytes.Repeat([]byte{0x04}, 64)...)...)
)

func multipartUpload(t *testing.T, filename string, data []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	field, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = field.Write(data)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return &buf, mw.FormDataContentType()
}

func doUpload(t *testing.T, handler http.Handler, authorization, query string, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartUpload(t, filename, data)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads"+query, body)
	req.Header.Set("Content-Type", contentType)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func newUploadFixture(t *testing.T) (http.Handler, string) {
	t.Helper()
	handler := New(testConfig(), storage.NewMemoryStore())
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Attach Squad"}, http.StatusCreated, &squad)
	return handler, squad.ID
}

func TestUploadImageAndServe(t *testing.T) {
	handler, squadID := newUploadFixture(t)

	rec := doUpload(t, handler, "", "?squad_id="+squadID, "bug screenshot.png", pngBytes)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var up uploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))
	require.NotEmpty(t, up.ID)
	require.Equal(t, squadID, up.SquadID)
	require.Equal(t, "bug screenshot.png", up.Filename)
	require.Equal(t, "image/png", up.ContentType)
	require.Equal(t, int64(len(pngBytes)), up.SizeBytes)
	require.Equal(t, "/api/v1/uploads/"+up.ID, up.URL)

	// Serve it back to an authorized user.
	req := httptest.NewRequest(http.MethodGet, up.URL, nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Equal(t, "image/png", rec2.Header().Get("Content-Type"))
	require.Equal(t, "nosniff", rec2.Header().Get("X-Content-Type-Options"))
	require.Contains(t, rec2.Header().Get("Cache-Control"), "private")
	require.Equal(t, pngBytes, rec2.Body.Bytes())
}

func TestUploadSniffsAllAcceptedTypes(t *testing.T) {
	handler, squadID := newUploadFixture(t)
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", pngBytes, "image/png"},
		{"gif", gifBytes, "image/gif"},
		{"jpeg", jpegBytes, "image/jpeg"},
		{"webp", webpBytes, "image/webp"},
	}
	for _, tc := range cases {
		rec := doUpload(t, handler, "", "?squad_id="+squadID, "x."+tc.name, tc.data)
		require.Equal(t, http.StatusCreated, rec.Code, tc.name+": "+rec.Body.String())
		var up uploadResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))
		require.Equal(t, tc.want, up.ContentType, tc.name)
	}
}

func TestUploadRejectsNonImageEvenWithImageExtension(t *testing.T) {
	handler, squadID := newUploadFixture(t)
	rec := doUpload(t, handler, "", "?squad_id="+squadID, "evil.png", []byte("#!/bin/sh\necho not an image\n"))
	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	require.Contains(t, rec.Body.String(), "unsupported_media_type")
}

func TestUploadRejectsOversize(t *testing.T) {
	handler, squadID := newUploadFixture(t)
	oversize := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x00}, int(maxUploadBytes))...)
	rec := doUpload(t, handler, "", "?squad_id="+squadID, "big.png", oversize)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), "upload_too_large")
}

func TestUploadRejectsEmptyAndMissingSquad(t *testing.T) {
	handler, squadID := newUploadFixture(t)

	rec := doUpload(t, handler, "", "?squad_id="+squadID, "empty.png", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "empty_file")

	rec = doUpload(t, handler, "", "", "x.png", pngBytes)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "missing_squad")
}

func TestUploadCrossSquadDenied(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authOwner:  {Email: "owner@example.com", Name: "Owner"},
		authViewer: {Email: "viewer@example.com", Name: "Viewer"},
	})

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Private Squad"}, http.StatusCreated, &squad)

	rec := doUpload(t, handler, authOwner, "?squad_id="+squad.ID, "secret.png", pngBytes)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var up uploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))

	// Viewer without any grant cannot fetch the image.
	req := httptest.NewRequest(http.MethodGet, up.URL, nil)
	req.Header.Set("Authorization", authViewer)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)
	require.Equal(t, http.StatusForbidden, rec2.Code)

	// Viewer cannot upload into someone else's squad either.
	rec3 := doUpload(t, handler, authViewer, "?squad_id="+squad.ID, "x.png", pngBytes)
	require.Equal(t, http.StatusForbidden, rec3.Code)
}

func TestChatMessageCarriesAttachments(t *testing.T) {
	handler, squadID := newUploadFixture(t)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squadID+"/agents", map[string]any{"name": "Visionary"}, http.StatusCreated, &agent)

	rec := doUpload(t, handler, "", "?squad_id="+squadID, "ui-bug.png", pngBytes)
	require.Equal(t, http.StatusCreated, rec.Code)
	var up uploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))

	var sent domain.Message
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/chat", map[string]any{
		"message":     "look at this broken button",
		"attachments": []string{up.ID},
	}, http.StatusCreated, &sent)

	var payload struct {
		Message     string           `json:"message"`
		Attachments []uploadResponse `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal(sent.Payload, &payload))
	require.Equal(t, "look at this broken button", payload.Message)
	require.Len(t, payload.Attachments, 1)
	require.Equal(t, up.ID, payload.Attachments[0].ID)
	require.Equal(t, "ui-bug.png", payload.Attachments[0].Filename)
	require.Equal(t, "image/png", payload.Attachments[0].ContentType)
	require.Equal(t, "/api/v1/uploads/"+up.ID, payload.Attachments[0].URL)

	// Chat history returns the same payload.
	var history []domain.Message
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/chat", nil, http.StatusOK, &history)
	require.Len(t, history, 1)
	require.Contains(t, string(history[0].Payload), `"attachments"`)
}

func TestChatMessageRejectsForeignSquadAttachment(t *testing.T) {
	handler, squadID := newUploadFixture(t)
	var other domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Other Squad"}, http.StatusCreated, &other)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squadID+"/agents", map[string]any{"name": "Worker"}, http.StatusCreated, &agent)

	rec := doUpload(t, handler, "", "?squad_id="+other.ID, "elsewhere.png", pngBytes)
	require.Equal(t, http.StatusCreated, rec.Code)
	var up uploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))

	// Referencing an upload from another squad (or a junk id) is rejected.
	doJSONNoBody(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/chat", map[string]any{
		"message":     "stolen bytes",
		"attachments": []string{up.ID},
	}, http.StatusBadRequest)
	doJSONNoBody(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/chat", map[string]any{
		"message":     "junk id",
		"attachments": []string{"not-a-uuid"},
	}, http.StatusBadRequest)
}

func TestTaskMessageCarriesAttachments(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	task := createBoardTask(t, h, f.squadID, "defect: button misaligned")
	assignTask(t, h, task.ID, f.workerID)

	rec := doUpload(t, h, "", "?squad_id="+f.squadID, "defect.png", pngBytes)
	require.Equal(t, http.StatusCreated, rec.Code)
	var up uploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))

	var sent domain.Message
	doJSON(t, h, http.MethodPost, pathTasksPrefix+task.ID+pathMessages, map[string]any{
		"message":     "screenshot of the defect",
		"attachments": []string{up.ID},
	}, http.StatusCreated, &sent)
	require.Contains(t, string(sent.Payload), `"attachments"`)
	require.Contains(t, string(sent.Payload), up.ID)
	require.Contains(t, string(sent.Payload), task.ID)

	// The thread renders the attachment for the task.
	var thread []domain.Message
	doJSON(t, h, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	found := false
	for _, m := range thread {
		if m.ID == sent.ID {
			require.Contains(t, string(m.Payload), "/api/v1/uploads/"+up.ID)
			found = true
		}
	}
	require.True(t, found, "task thread must include the attachment URL")
}

func TestAgentFetchesOwnSquadUpload(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	rec := doUpload(t, h, "", "?squad_id="+f.squadID, "agent-view.png", gifBytes)
	require.Equal(t, http.StatusCreated, rec.Code)
	var up uploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))

	// The receiving agent can pull the bytes with its own credential.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/me/uploads/"+up.ID, nil)
	req.Header.Set("Authorization", bearerPrefix+f.workerCred)
	req.Header.Set("X-Skquad-Agent-ID", f.workerID)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Equal(t, "image/gif", rec2.Header().Get("Content-Type"))
	require.Equal(t, gifBytes, rec2.Body.Bytes())

	// An upload from another squad is forbidden to this agent.
	var otherSquad domain.Squad
	doJSON(t, h, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Foreign Squad"}, http.StatusCreated, &otherSquad)
	recOther := doUpload(t, h, "", "?squad_id="+otherSquad.ID, "nope.png", pngBytes)
	require.Equal(t, http.StatusCreated, recOther.Code)
	var otherUp uploadResponse
	require.NoError(t, json.Unmarshal(recOther.Body.Bytes(), &otherUp))

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/agents/me/uploads/"+otherUp.ID, nil)
	req2.Header.Set("Authorization", bearerPrefix+f.workerCred)
	req2.Header.Set("X-Skquad-Agent-ID", f.workerID)
	rec4 := httptest.NewRecorder()
	h.ServeHTTP(rec4, req2)
	require.Equal(t, http.StatusForbidden, rec4.Code)
}

func TestDetectImageContentType(t *testing.T) {
	require.Equal(t, "image/png", detectImageContentType(pngBytes))
	require.Equal(t, "image/gif", detectImageContentType(gifBytes))
	require.Equal(t, "image/jpeg", detectImageContentType(jpegBytes))
	require.Equal(t, "image/webp", detectImageContentType(webpBytes))
	require.Equal(t, "", detectImageContentType([]byte("plain text, definitely not an image")))
	require.Equal(t, "", detectImageContentType(nil))
}

func TestSanitizeUploadFilename(t *testing.T) {
	require.Equal(t, "evil.png", sanitizeUploadFilename("../../etc/evil.png"))
	require.Equal(t, "ok name.png", sanitizeUploadFilename("ok name.png"))
	require.Equal(t, "ab", sanitizeUploadFilename("a\x00b"))
	require.Equal(t, "image", sanitizeUploadFilename("   "))
}
