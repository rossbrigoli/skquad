package httpapi

// S-216: inbox attachment endpoint tests — multipart send_inbox with
// files, server-side executable rejection, authenticated downloads,
// recipient-only authorization, and list enrichment.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// multipartSendInbox builds the agent's send_inbox multipart body:
// text fields + N "attachments" file parts.
func multipartSendInbox(t *testing.T, fields map[string]string, files [][2]string) (bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	for _, f := range files { // f = {filename, content}
		part, err := mw.CreateFormFile("attachments", f[0])
		require.NoError(t, err)
		_, err = part.Write([]byte(f[1]))
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return buf, mw.FormDataContentType()
}

func doAgentMultipart(t *testing.T, handler http.Handler, agentID, credential string, fields map[string]string, files [][2]string) *httptest.ResponseRecorder {
	t.Helper()
	buf, ct := multipartSendInbox(t, fields, files)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/me/inbox", &buf)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("X-Skquad-Agent-ID", agentID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestS216SendInboxWithTextAttachment(t *testing.T) {
	handler, _, _, agent, credential := s216Setup(t, "s216-text")

	rec := doAgentMultipart(t, handler, agent.ID, credential,
		map[string]string{"message": "Your financial report is ready", "subject": "Q3 report"},
		[][2]string{{"q3-report.txt", "revenue: 42, costs: 7\n"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created domain.InboxMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Len(t, created.Attachments, 1)
	att := created.Attachments[0]
	require.Equal(t, "q3-report.txt", att.Filename)
	require.Equal(t, "text/plain", att.ContentType)
	require.EqualValues(t, len("revenue: 42, costs: 7\n"), att.SizeBytes)
	require.Len(t, att.SHA256, 64)
	require.Equal(t, fmt.Sprintf("/api/v1/inbox/%s/attachments/%s", created.ID, att.ID), att.URL)

	// Recipient downloads it.
	req := httptest.NewRequest(http.MethodGet, att.URL, nil)
	dl := httptest.NewRecorder()
	handler.ServeHTTP(dl, req)
	require.Equal(t, http.StatusOK, dl.Code)
	require.Equal(t, "text/plain", dl.Header().Get("Content-Type"))
	require.Contains(t, dl.Header().Get("Content-Disposition"), "attachment")
	require.Equal(t, "revenue: 42, costs: 7\n", dl.Body.String())
	require.Equal(t, "nosniff", dl.Header().Get("X-Content-Type-Options"))
}

func TestS216SendInboxRejectsExecutableBinary(t *testing.T) {
	handler, _, _, agent, credential := s216Setup(t, "s216-reject")

	// A copy of /bin/ls's first bytes: ELF magic.
	elf := append([]byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}, bytes.Repeat([]byte{0x2a}, 256)...)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("message", "sneaky"))
	part, err := mw.CreateFormFile("attachments", "report.pdf")
	require.NoError(t, err)
	_, err = part.Write(elf)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/me/inbox", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("X-Skquad-Agent-ID", agent.ID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Equal(t, "attachment_rejected", errBody.Error.Code)
	require.Contains(t, errBody.Error.Message, "ELF")

	// Nothing was delivered: the owner's inbox is unchanged.
	var msgs []*domain.InboxMessage
	doJSON(t, handler, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &msgs)
	require.Empty(t, msgs)
}

func TestS216SendInboxRejectsTooManyAttachments(t *testing.T) {
	handler, _, _, agent, credential := s216Setup(t, "s216-many")
	files := make([][2]string, domain.MaxInboxAttachmentsPerMsg+1)
	for i := range files {
		files[i] = [2]string{fmt.Sprintf("f%d.txt", i), "text"}
	}
	rec := doAgentMultipart(t, handler, agent.ID, credential, map[string]string{"message": "lots"}, files)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "too_many_attachments")
}

func TestS216AttachmentDownloadAuthorization(t *testing.T) {
	// OIDC mode with header-keyed profiles so we can act as two
	// distinct non-admin humans (dev mode force-promotes its principal
	// to platform_admin on every request).
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	profiles := headerOIDC{
		"Bearer owner": {
			Issuer: testIssuer, Subject: "owner-1", Email: "owner@example.com",
			EmailVerified: true, Name: "Owner Human",
		},
		"Bearer intruder": {
			Issuer: testIssuer, Subject: "intruder-1", Email: "intruder@example.com",
			EmailVerified: true, Name: "Intruder Human",
		},
	}
	crWriter := &fakeCRWriter{}
	handler := NewWithDependencies(cfg, store, profiles, crWriter, nil)

	// Owner builds a squad with an agent.
	var squad domain.Squad
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, pathSquads, map[string]any{"name": "s216-authz"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "worker"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, credential)

	// Agent delivers a message + attachment to the squad owner.
	rec := doAgentMultipart(t, handler, agent.ID, credential,
		map[string]string{"message": "for the owner"},
		[][2]string{{"owner-notes.md", "# owner only\n"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created domain.InboxMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	att := created.Attachments[0]

	// The recipient downloads their own attachment.
	ownReq := httptest.NewRequest(http.MethodGet, att.URL, nil)
	ownReq.Header.Set("Authorization", "Bearer owner")
	ownRec := httptest.NewRecorder()
	handler.ServeHTTP(ownRec, ownReq)
	require.Equal(t, http.StatusOK, ownRec.Code)

	// Another human must NOT be able to list or download it.
	lrec := httptest.NewRequest(http.MethodGet, "/api/v1/inbox/"+created.ID+"/attachments", nil)
	lrec.Header.Set("Authorization", "Bearer intruder")
	lrr := httptest.NewRecorder()
	handler.ServeHTTP(lrr, lrec)
	require.Equal(t, http.StatusForbidden, lrr.Code)

	drec := httptest.NewRequest(http.MethodGet, att.URL, nil)
	drec.Header.Set("Authorization", "Bearer intruder")
	drr := httptest.NewRecorder()
	handler.ServeHTTP(drr, drec)
	require.Equal(t, http.StatusForbidden, drr.Code)

	// Cross-message attachment ids never serve bytes: the handler-level
	// guard rejects a message/attachment mismatch.
	xrec := httptest.NewRequest(http.MethodGet, "/api/v1/inbox/"+created.ID+"/attachments/"+created.ID, nil)
	xrec.Header.Set("Authorization", "Bearer owner")
	xrr := httptest.NewRecorder()
	handler.ServeHTTP(xrr, xrec)
	require.NotEqual(t, http.StatusOK, xrr.Code)

	// Platform admin override: promote the intruder and the download
	// succeeds (mirrors the admin inbox filter).
	var intruder domain.User
	doJSONAuth(t, handler, "Bearer intruder", http.MethodGet, pathAuthMe, nil, http.StatusOK, &intruder)
	require.NoError(t, store.SetUserRole(context.Background(), intruder.ID, domain.RolePlatformAdmin))
	arec := httptest.NewRequest(http.MethodGet, att.URL, nil)
	arec.Header.Set("Authorization", "Bearer intruder")
	arr := httptest.NewRecorder()
	handler.ServeHTTP(arr, arec)
	require.Equal(t, http.StatusOK, arr.Code)
}

func TestS216InboxListEnrichmentAndCascade(t *testing.T) {
	handler, store, _, agent, credential := s216Setup(t, "s216-list")

	rec := doAgentMultipart(t, handler, agent.ID, credential,
		map[string]string{"message": "two files"},
		[][2]string{{"a.txt", "ZZ-not-leaked-zz"}, {"b.csv", "x,y\n1,2\n"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created domain.InboxMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Len(t, created.Attachments, 2)

	var msgs []*domain.InboxMessage
	doJSON(t, handler, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &msgs)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Attachments, 2)
	require.NotEmpty(t, msgs[0].Attachments[0].URL)
	// Listing never leaks bytes. The marker uses non-hex characters so it can
	// never collide with a randomly generated UUID in the marshalled JSON
	// (a previous "aaa" marker flaked by matching inside an attachment id).
	raw, _ := json.Marshal(msgs)
	require.NotContains(t, string(raw), "ZZ-not-leaked-zz")

	// Deleting the message cascades the attachments away.
	del := httptest.NewRequest(http.MethodDelete, "/api/v1/inbox/"+created.ID, nil)
	drr := httptest.NewRecorder()
	handler.ServeHTTP(drr, del)
	require.Equal(t, http.StatusNoContent, drr.Code)

	_, err := store.GetInboxAttachment(context.Background(), created.Attachments[0].ID)
	require.ErrorIs(t, err, storage.ErrNotFound)
}

// s216Setup mirrors agentRuntimeSetup but keeps the *MemoryStore handle
// so tests can drive storage-level scenarios (second users, cascade
// verification) directly.
func s216Setup(t *testing.T, squadName string) (http.Handler, *storage.MemoryStore, domain.Squad, domain.Agent, string) {
	t.Helper()
	store := storage.NewMemoryStore()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": squadName}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "worker"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, credential)
	return handler, store, squad, agent, credential
}
