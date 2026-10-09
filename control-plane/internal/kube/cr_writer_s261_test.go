package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// S-261: the CR writer mirrors the control-plane-fixed workspace PVC name
// into spec.workspacePVCName — but only when storage is enabled.

func newCaptureWriter(t *testing.T, gotBody *map[string]any) *CRWriter {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return &CRWriter{
		baseURL:            server.URL,
		namespace:          testNamespace,
		groupVersion:       testAPIVersion,
		agentImage:         runtimeImageRef,
		defaultStorageSize: "2Gi",
		token:              testToken,
		client:             server.Client(),
	}
}

func TestUpsertAgentEmitsWorkspacePVCNameWhenStorageEnabled(t *testing.T) {
	t.Parallel()

	friendly := "ross-brigoli-minions-bob-workspace-d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c"
	var gotBody map[string]any
	writer := newCaptureWriter(t, &gotBody)

	agent := &domain.Agent{
		ID:               "d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c",
		SquadID:          testSquadName,
		Role:             "coder",
		IdleTimeoutSec:   300,
		StorageEnabled:   true,
		WorkspacePVCName: friendly,
	}
	if err := writer.UpsertAgent(context.Background(), agent, nil); err != nil {
		t.Fatal(err)
	}
	spec := gotBody["spec"].(map[string]any)
	if spec["workspacePVCName"] != friendly {
		t.Fatalf("spec.workspacePVCName = %v, want %q", spec["workspacePVCName"], friendly)
	}
}

func TestUpsertAgentOmitsWorkspacePVCNameWhenStorageDisabled(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	writer := newCaptureWriter(t, &gotBody)

	agent := &domain.Agent{
		ID:               "d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c",
		SquadID:          testSquadName,
		Role:             "coder",
		IdleTimeoutSec:   300,
		StorageEnabled:   false,
		WorkspacePVCName: "ross-brigoli-minions-bob-workspace-d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c",
	}
	if err := writer.UpsertAgent(context.Background(), agent, nil); err != nil {
		t.Fatal(err)
	}
	spec := gotBody["spec"].(map[string]any)
	if _, present := spec["workspacePVCName"]; present {
		t.Fatalf("spec.workspacePVCName must be absent when storage disabled")
	}
}

func TestUpsertAgentOmitsWorkspacePVCNameWhenUnset(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	writer := newCaptureWriter(t, &gotBody)

	// Pre-S-261 agent: storage on, no fixed name — the operator keeps
	// its legacy derivation, so the field must not appear.
	agent := &domain.Agent{
		ID:             "d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c",
		SquadID:        testSquadName,
		Role:           "coder",
		IdleTimeoutSec: 300,
		StorageEnabled: true,
	}
	if err := writer.UpsertAgent(context.Background(), agent, nil); err != nil {
		t.Fatal(err)
	}
	spec := gotBody["spec"].(map[string]any)
	if _, present := spec["workspacePVCName"]; present {
		t.Fatalf("spec.workspacePVCName must be absent when the agent has no fixed name")
	}
}
