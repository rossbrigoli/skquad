package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

// --- S-261: friendly workspace PVC naming (spec.workspacePVCName) ---

func TestWorkspacePVCNamePrefersSpecField(t *testing.T) {
	t.Parallel()

	friendly := "ross-brigoli-minions-bob-workspace-d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c"
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-cr-name"},
		Spec:       skquadv1.AgentSpec{WorkspacePVCName: friendly},
	}
	if got := workspacePVCName(agent); got != friendly {
		t.Fatalf("spec name must win: got %q, want %q", got, friendly)
	}

	// Whitespace-only is treated as empty (falls back).
	agent.Spec.WorkspacePVCName = "   "
	if got := workspacePVCName(agent); got != "agent-agent-cr-name-workspace" {
		t.Fatalf("blank spec: got %q, want legacy fallback", got)
	}
}

func TestWorkspacePVCNameLegacyFallbackForPreS261CRs(t *testing.T) {
	t.Parallel()

	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-legacy-1"},
		Spec:       skquadv1.AgentSpec{},
	}
	if got := workspacePVCName(agent); got != "agent-agent-legacy-1-workspace" {
		t.Fatalf("legacy fallback: got %q, want agent-agent-legacy-1-workspace", got)
	}
}

// TestReconcileUsesSpecWorkspacePVCName proves the pod claim and the PVC
// actually created agree when the control plane supplies a friendly name,
// and that the legacy name is NOT created alongside it.
func TestReconcileUsesSpecWorkspacePVCName(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	friendly := "ross-brigoli-minions-bob-workspace-d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c"
	squad, agent := pvcAgent("agent-s261", "aaaa0026-0000-0000-0000-000000002601", "squad-s261",
		&skquadv1.AgentStorage{Enabled: true})
	agent.Spec.WorkspacePVCName = friendly
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent, s104Secret("squad-s261", agentCredS104), s104Secret("squad-s261", agentVKeyS104)).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	reconcileAgent(t, reconciler, agent)
	reconcileAgent(t, reconciler, agent)

	var pvc corev1.PersistentVolumeClaim
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "squad-s261", Name: friendly}, &pvc); err != nil {
		t.Fatalf("friendly PVC not created: %v", err)
	}
	// The legacy name must NOT exist — no shadow volume.
	var legacy corev1.PersistentVolumeClaim
	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "squad-s261", Name: "agent-" + agent.Name + "-workspace"}, &legacy)
	if err == nil {
		t.Fatalf("legacy-named PVC unexpectedly created alongside friendly name")
	}

	// Pod claim must match the PVC that was created.
	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squad.Spec.Namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	var wsVolume *corev1.Volume
	for i := range deployment.Spec.Template.Spec.Volumes {
		if deployment.Spec.Template.Spec.Volumes[i].Name == workspaceVolumeName {
			wsVolume = &deployment.Spec.Template.Spec.Volumes[i]
		}
	}
	if wsVolume == nil || wsVolume.PersistentVolumeClaim == nil {
		t.Fatalf("workspace volume missing")
	}
	if wsVolume.PersistentVolumeClaim.ClaimName != friendly {
		t.Fatalf("pod claim = %q, want %q", wsVolume.PersistentVolumeClaim.ClaimName, friendly)
	}
	// PORTABILITY RULE still intact (S-135): empty storageClass => omitted.
	if pvc.Spec.StorageClassName != nil {
		t.Fatalf("storageClassName = %q, want nil (cluster default)", *pvc.Spec.StorageClassName)
	}
}

// TestDeleteUsesSpecWorkspacePVCName: agent deletion removes the PVC under
// the friendly name (the delete path reads the same function).
func TestDeleteUsesSpecWorkspacePVCName(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	friendly := "ross-brigoli-minions-bob-workspace-11111111-2222-3333-4444-555555555555"
	squad, agent := pvcAgent("agent-s261-del", "aaaa0026-0000-0000-0000-000000002602", "squad-s261-del",
		&skquadv1.AgentStorage{Enabled: true})
	agent.Spec.WorkspacePVCName = friendly
	existing := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      friendly,
			Namespace: "squad-s261-del",
			Labels:    map[string]string{LabelWorkspacePVC: "true"},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent, existing).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	// Delete the agent CR with the finalizer present, then reconcile to
	// run the finalizer's cleanup.
	if err := k8sClient.Delete(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	reconcileAgent(t, reconciler, agent)

	var pvc corev1.PersistentVolumeClaim
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "squad-s261-del", Name: friendly}, &pvc); err == nil {
		t.Fatalf("friendly PVC %q should have been deleted with the agent", friendly)
	}
}
