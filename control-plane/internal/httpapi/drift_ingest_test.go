// TG-11 slice D tests: drift-report ingest (auth, storage, validation),
// the batched owner digest (one inbox message per resource per UTC day,
// never per-host), and the artifact-resources read surface backing the
// terminal-service drift runner.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const driftTestToken = "drift-ingest-test-token"

func driftJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func driftBearer(token string) string { return "Bearer " + token }

// driftSeed creates a handler with the drift token configured plus an
// owner user and one artifact-enabled ssh resource with two host groups.
func driftSeed(t *testing.T, driftToken string) (http.Handler, *storage.MemoryStore, string, string) {
	t.Helper()
	store := storage.NewMemoryStore()
	cfg := testConfig()
	cfg.DriftIngestToken = driftToken
	handler := New(cfg, store)
	ctx := context.Background()

	owner, err := store.UpsertUser(ctx, &domain.User{Email: "drift-owner@example.com", Name: "Drift Owner"})
	require.NoError(t, err)

	res, err := store.CreateResource(ctx, &domain.RegistryResource{
		Type:        domain.ResSSH,
		Name:        "web-servers",
		OwnerUserID: owner.ID,
		EndpointConfig: driftJSON(map[string]any{
			"ssh_user":    "deploy",
			"known_hosts": "web1.example.com ssh-ed25519 AAAAkey",
			"artifact": map[string]any{
				"git_url":        "https://git.example.com/ops/playbooks.git",
				"default_branch": "main",
				"playbooks_path": "playbooks",
				"drift_playbook": "site.yml",
			},
		}),
		PolicyCeiling: driftJSON(map[string]any{
			"hosts_allow": []string{"web1.example.com", "web2.example.com", "db1.example.com"},
			"host_groups": map[string]any{
				"web": map[string]any{"hosts": []string{"web1.example.com", "web2.example.com"}, "tier": "medium"},
				"db":  map[string]any{"hosts": []string{"db1.example.com"}, "tier": "high"},
			},
		}),
	})
	require.NoError(t, err)
	return handler, store, owner.ID, res.ID
}

func driftBody(resourceID string, drifted []string) map[string]any {
	return map[string]any{
		"resource_id":   resourceID,
		"host_group":    "web",
		"playbook":      "site.yml",
		"git_rev":       "0123456789abcdef0123456789abcdef01234567",
		"drifted_hosts": drifted,
	}
}

func TestDriftIngestAuthRequired(t *testing.T) {
	handler, _, _, resID := driftSeed(t, driftTestToken)

	rec := doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports",
		string(driftJSON(driftBody(resID, []string{"web1.example.com"}))), "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports",
		string(driftJSON(driftBody(resID, nil))), "Bearer wrong-token")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Unconfigured surface fails closed with 503, not open.
	unconfigured := New(testConfig(), storage.NewMemoryStore())
	rec = doRaw(t, unconfigured, http.MethodPost, "/internal/v1/drift-reports",
		string(driftJSON(driftBody(resID, nil))), driftBearer(driftTestToken))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestDriftIngestStoresReportAndFilesDigest(t *testing.T) {
	handler, store, ownerID, resID := driftSeed(t, driftTestToken)
	ctx := context.Background()

	rec := doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports",
		string(driftJSON(driftBody(resID, []string{"web2.example.com", "web1.example.com", "web1.example.com"}))),
		driftBearer(driftTestToken))
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotEmpty(t, out["id"])
	require.Equal(t, false, out["in_sync"])
	require.Equal(t, true, out["digest_created"])

	n, err := store.CountDriftReports(ctx, resID, utcDayStart(time.Now()), true)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	msgs, err := store.ListInboxMessages(ctx, ownerID, false, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, domain.InboxActionRequired, msgs[0].Kind)
	require.Contains(t, msgs[0].Body, "web1.example.com, web2.example.com")
	require.Contains(t, msgs[0].Body, "site.yml")
}

func TestDriftIngestDigestBatchedPerResourcePerDay(t *testing.T) {
	handler, store, ownerID, resID := driftSeed(t, driftTestToken)
	ctx := context.Background()

	// First drift of the day → digest.
	rec := doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports",
		string(driftJSON(driftBody(resID, []string{"web1.example.com"}))), driftBearer(driftTestToken))
	require.Equal(t, http.StatusOK, rec.Code)
	var first map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &first))
	require.Equal(t, true, first["digest_created"])

	// Second drift same day (different group) → row recorded, NO new message.
	body2 := driftBody(resID, []string{"db1.example.com"})
	body2["host_group"] = "db"
	rec = doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports", string(driftJSON(body2)), driftBearer(driftTestToken))
	require.Equal(t, http.StatusOK, rec.Code)
	var second map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &second))
	require.Equal(t, false, second["digest_created"])

	n, err := store.CountDriftReports(ctx, resID, utcDayStart(time.Now()), true)
	require.NoError(t, err)
	require.Equal(t, 2, n, "every check run is a row")

	msgs, err := store.ListInboxMessages(ctx, ownerID, false, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1, "one batched digest per resource per day, never per-host")
}

func TestDriftIngestInSyncNoDigest(t *testing.T) {
	handler, store, ownerID, resID := driftSeed(t, driftTestToken)
	ctx := context.Background()

	rec := doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports",
		string(driftJSON(driftBody(resID, []string{}))), driftBearer(driftTestToken))
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, true, out["in_sync"])
	require.Equal(t, false, out["digest_created"])

	total, err := store.CountDriftReports(ctx, resID, utcDayStart(time.Now()), false)
	require.NoError(t, err)
	require.Equal(t, 1, total, "in_sync report is still recorded")
	drifted, err := store.CountDriftReports(ctx, resID, utcDayStart(time.Now()), true)
	require.NoError(t, err)
	require.Equal(t, 0, drifted)

	msgs, err := store.ListInboxMessages(ctx, ownerID, false, 10)
	require.NoError(t, err)
	require.Empty(t, msgs)
}

func TestDriftIngestValidation(t *testing.T) {
	handler, _, _, resID := driftSeed(t, driftTestToken)

	bad := driftBody(resID, []string{"web1.example.com"})
	bad["git_rev"] = "deadbeef" // not a full SHA
	rec := doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports", string(driftJSON(bad)), driftBearer(driftTestToken))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	missing := map[string]any{"resource_id": resID}
	rec = doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports", string(driftJSON(missing)), driftBearer(driftTestToken))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports", "{not json", driftBearer(driftTestToken))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doRaw(t, handler, http.MethodPost, "/internal/v1/drift-reports",
		string(driftJSON(driftBody("nope-does-not-exist", []string{"h"}))), driftBearer(driftTestToken))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListDriftArtifactResources(t *testing.T) {
	handler, store, ownerID, resID := driftSeed(t, driftTestToken)
	ctx := context.Background()

	// Non-checkable siblings: artifact without host groups, and no artifact.
	_, err := store.CreateResource(ctx, &domain.RegistryResource{
		Type: domain.ResSSH, Name: "no-groups", OwnerUserID: ownerID,
		EndpointConfig: driftJSON(map[string]any{"ssh_user": "x", "artifact": map[string]any{"git_url": "https://git.example.com/a/b.git"}}),
		PolicyCeiling:  driftJSON(map[string]any{"hosts_allow": []string{"h1"}}),
	})
	require.NoError(t, err)
	_, err = store.CreateResource(ctx, &domain.RegistryResource{
		Type: domain.ResSSH, Name: "plain-ssh", OwnerUserID: ownerID,
		EndpointConfig: driftJSON(map[string]any{"ssh_user": "x"}),
		PolicyCeiling:  driftJSON(map[string]any{"hosts_allow": []string{"h2"}, "host_groups": map[string]any{"g": map[string]any{"hosts": []string{"h2"}}}}),
	})
	require.NoError(t, err)

	rec := doRaw(t, handler, http.MethodGet, "/internal/v1/artifact-resources", "", driftBearer(driftTestToken))
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Resources []map[string]any `json:"resources"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Resources, 1, "only artifact+host_groups resources are drift-checkable")

	got := out.Resources[0]
	require.Equal(t, resID, got["resource_id"])
	require.Equal(t, "https://git.example.com/ops/playbooks.git", got["git_url"])
	require.Equal(t, "main", got["default_branch"])
	require.Equal(t, "playbooks", got["playbooks_path"])
	require.Equal(t, "site.yml", got["drift_playbook"])
	require.Equal(t, "deploy", got["ssh_user"])
	require.Equal(t, "web1.example.com ssh-ed25519 AAAAkey", got["known_hosts"])

	groups, ok := got["host_groups"].([]any)
	require.True(t, ok)
	require.Len(t, groups, 2)
	// Sorted by name: db before web.
	require.Equal(t, "db", groups[0].(map[string]any)["name"])
	require.Equal(t, "web", groups[1].(map[string]any)["name"])

	// Auth applies to the read surface too.
	rec = doRaw(t, handler, http.MethodGet, "/internal/v1/artifact-resources", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
