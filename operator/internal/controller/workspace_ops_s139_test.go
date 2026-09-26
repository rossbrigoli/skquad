package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

// --- S-139: storage ops — size cap admission, workspace label, orphan GC ---

func TestWorkspaceSizeCapBlocksOversizedPVC(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-cap-big", "aaaa1001-0000-0000-0000-000000000001", "squad-cap-big",
		&skquadv1.AgentStorage{Enabled: true, Size: "50Gi"})
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	reconcileAgent(t, reconciler, agent)

	cond := getCondition(t, k8sClient, types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}, "WorkspaceReady")
	if cond == nil {
		t.Fatal("expected WorkspaceReady condition")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "StorageSizeExceedsPlatformMax" {
		t.Fatalf("WorkspaceReady = %s/%s, want False/StorageSizeExceedsPlatformMax", cond.Status, cond.Reason)
	}
	// The oversized claim must NOT exist — admission blocks creation entirely.
	err := k8sClient.Get(context.Background(),
		client.ObjectKey{Namespace: "squad-cap-big", Name: workspacePVCName(agent)},
		&corev1.PersistentVolumeClaim{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("oversized PVC was created (err=%v), want absent", err)
	}
}

func TestWorkspaceSizeCapEnvOverrideAllows(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel — this test is serial by design.
	t.Setenv(envMaxAgentStorage, "20Gi")

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-cap-env", "aaaa1002-0000-0000-0000-000000000002", "squad-cap-env",
		&skquadv1.AgentStorage{Enabled: true, Size: "15Gi"})
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	reconcileAgent(t, reconciler, agent)

	pvc := getWorkspacePVC(t, k8sClient, "squad-cap-env", agent.Name)
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "15Gi" {
		t.Fatalf("PVC size = %q, want 15Gi under raised cap", got)
	}
	cond := getCondition(t, k8sClient, types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}, "WorkspaceReady")
	if cond != nil && cond.Reason == "StorageSizeExceedsPlatformMax" {
		t.Fatalf("raised cap was not honored: %s", cond.Message)
	}
}

func TestWorkspacePVCCarriesWorkspaceLabelAndMountEnv(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-cap-label", "aaaa1003-0000-0000-0000-000000000003", "squad-cap-label",
		&skquadv1.AgentStorage{Enabled: true, MountPath: "/data"})
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent,
			s104Secret("squad-cap-label", agentCredS104),
			s104Secret("squad-cap-label", agentVKeyS104)).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	reconcileAgent(t, reconciler, agent)

	pvc := getWorkspacePVC(t, k8sClient, "squad-cap-label", agent.Name)
	if pvc.Labels[LabelWorkspacePVC] != "true" {
		t.Fatalf("workspace-pvc label = %q, want \"true\"", pvc.Labels[LabelWorkspacePVC])
	}

	// S-136 follow-up: non-default mountPath must be injected as
	// SKQUAD_WORKSPACE_MOUNT_PATH so runtime resolution matches the mount.
	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "squad-cap-label", Name: agent.Name}, &deployment); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "SKQUAD_WORKSPACE_MOUNT_PATH" {
			found = true
			if env.Value != "/data" {
				t.Fatalf("SKQUAD_WORKSPACE_MOUNT_PATH = %q, want /data", env.Value)
			}
		}
	}
	if !found {
		t.Fatal("SKQUAD_WORKSPACE_MOUNT_PATH not injected into agent container")
	}
}

func TestAdoptedPVCGetsWorkspaceLabelMigrated(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-cap-adopt", "aaaa1004-0000-0000-0000-000000000004", "squad-cap-adopt",
		&skquadv1.AgentStorage{Enabled: true})
	// Pre-existing claim from a pre-S-139 operator: no workspace-pvc label.
	legacy := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workspacePVCName(agent),
			Namespace: "squad-cap-adopt",
			Labels:    map[string]string{LabelAgentID: agent.Spec.AgentID},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent, legacy).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	reconcileAgent(t, reconciler, agent)

	pvc := getWorkspacePVC(t, k8sClient, "squad-cap-adopt", agent.Name)
	if pvc.UID != legacy.UID {
		t.Fatal("adopted PVC was replaced instead of labeled in place")
	}
	if pvc.Labels[LabelWorkspacePVC] != "true" {
		t.Fatalf("legacy PVC was not migrated with workspace-pvc label: %v", pvc.Labels)
	}
}

func gcPVC(name, namespace, agentID string, retain bool) *corev1.PersistentVolumeClaim {
	labels := map[string]string{
		LabelWorkspacePVC: "true",
		LabelAgentID:      agentID,
	}
	annotations := map[string]string{}
	if retain {
		annotations[RetainPVCKeepAnnotation] = "true"
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
	}
}

func pvcExists(t *testing.T, c client.Client, namespace, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &corev1.PersistentVolumeClaim{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func TestOrphanPVCGCDeletesOrphansKeepsEverythingElse(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-gc", Namespace: testNamespace},
		Spec:       skquadv1.SquadSpec{SquadID: "bbbb1000-0000-0000-0000-000000000010", Namespace: "squad-gc"},
	}
	liveAgent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-live", Namespace: testNamespace},
		Spec:       skquadv1.AgentSpec{AgentID: "live-agent", SquadID: squad.Spec.SquadID},
	}
	// Deployment still referencing an otherwise-orphan claim (guard 4).
	mounter := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-ref", Namespace: "squad-gc"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{{
						Name:         "workspace",
						VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "agent-ref-workspace"}},
					}},
				},
			},
		},
	}
	orphan := gcPVC("agent-dead-workspace", "squad-gc", "dead-agent", false)
	retained := gcPVC("agent-kept-workspace", "squad-gc", "kept-agent", true)
	livePVC := gcPVC("agent-live-workspace", "squad-gc", "live-agent", false)
	referenced := gcPVC("agent-ref-workspace", "squad-gc", "gone-agent", false)
	unlabeled := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-pvc", Namespace: "squad-gc"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, liveAgent, mounter, orphan, retained, livePVC, referenced, unlabeled).
		Build()
	gc := &OrphanPVCGC{Client: k8sClient}

	report, err := gc.SweepOrphanWorkspacePVCs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 4 {
		t.Fatalf("scanned = %d, want 4 (unlabeled PVC must not be scanned)", report.Scanned)
	}
	if report.Cleaned != 1 || report.Retained != 1 || report.SkippedInUse != 1 {
		t.Fatalf("report = %+v, want cleaned=1 retained=1 skippedInUse=1", report)
	}
	if pvcExists(t, k8sClient, "squad-gc", "agent-dead-workspace") {
		t.Fatal("orphan PVC was not deleted")
	}
	for _, name := range []string{"agent-kept-workspace", "agent-live-workspace", "agent-ref-workspace", "someone-elses-pvc"} {
		if !pvcExists(t, k8sClient, "squad-gc", name) {
			t.Fatalf("%s must survive the sweep", name)
		}
	}
}

func TestOrphanPVCGCLeavesUnmanagedNamespacesAlone(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	// A labeled orphan in a namespace with NO Squad CR: not platform-
	// managed, the GC must never touch it.
	stray := gcPVC("agent-stray-workspace", "not-a-squad-ns", "stray-agent", false)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stray).Build()
	gc := &OrphanPVCGC{Client: k8sClient}

	report, err := gc.SweepOrphanWorkspacePVCs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 0 || report.Cleaned != 0 {
		t.Fatalf("report = %+v, want zero scan outside managed namespaces", report)
	}
	if !pvcExists(t, k8sClient, "not-a-squad-ns", "agent-stray-workspace") {
		t.Fatal("GC touched a PVC outside a squad namespace")
	}
}
