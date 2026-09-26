package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

// --- S-135: per-agent durable workspace PVC ---

// pvcAgent builds an active, credentialed agent with a storage block.
func pvcAgent(name, squadID, squadNS string, storage *skquadv1.AgentStorage) (*skquadv1.Squad, *skquadv1.Agent) {
	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-" + name, Namespace: testNamespace},
		Spec:       skquadv1.SquadSpec{SquadID: squadID, Namespace: squadNS},
	}
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  testNamespace,
			Finalizers: []string{agentFinalizer},
		},
		Spec: skquadv1.AgentSpec{
			AgentID:          "pvc-" + squadID,
			SquadID:          squadID,
			Image:            testAgentImageRef,
			CredentialSecret: agentCredS104,
			VirtualKeySecret: agentVKeyS104,
			IdleTimeout:      "300s",
			DesiredActive:    true,
			Storage:          storage,
		},
	}
	return squad, agent
}

func reconcileAgent(t *testing.T, r *AgentReconciler, agent *skquadv1.Agent) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func getWorkspacePVC(t *testing.T, c client.Client, namespace, agentName string) corev1.PersistentVolumeClaim {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "agent-" + agentName + "-workspace"}, &pvc); err != nil {
		t.Fatal(err)
	}
	return pvc
}

func getCondition(t *testing.T, c client.Client, key types.NamespacedName, condType string) *metav1.Condition {
	t.Helper()
	agent := getAgentStatus(t, c, key)
	for i := range agent.Status.Conditions {
		if agent.Status.Conditions[i].Type == condType {
			return &agent.Status.Conditions[i]
		}
	}
	return nil
}

func boundPVC(namespace, agentName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-" + agentName + "-workspace",
			Namespace: namespace,
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func TestWorkspacePVCCreatedWithDefaultsAndIdempotent(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-pvc-def", "aaaa0001-0000-0000-0000-000000000001", "squad-pvc-def",
		&skquadv1.AgentStorage{Enabled: true})
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent, s104Secret("squad-pvc-def", agentCredS104), s104Secret("squad-pvc-def", agentVKeyS104)).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	// Two reconciles: the PVC must be created exactly once and the second
	// pass must adopt it without error (idempotent).
	reconcileAgent(t, reconciler, agent)
	reconcileAgent(t, reconciler, agent)

	pvc := getWorkspacePVC(t, k8sClient, "squad-pvc-def", agent.Name)
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "2Gi" {
		t.Fatalf("PVC size = %q, want 2Gi default", got)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("access modes = %v, want [ReadWriteOnce]", pvc.Spec.AccessModes)
	}
	// PORTABILITY RULE: empty storageClass => storageClassName omitted.
	if pvc.Spec.StorageClassName != nil {
		t.Fatalf("storageClassName = %q, want nil (cluster default) when storageClass empty", *pvc.Spec.StorageClassName)
	}
	if pvc.Labels[LabelAgentID] != agent.Spec.AgentID {
		t.Fatalf("PVC agent-id label = %q, want %q", pvc.Labels[LabelAgentID], agent.Spec.AgentID)
	}

	// A third reconcile must not error or duplicate (CreateOrUpdate-style
	// idempotency: the existing claim is adopted as-is).
	reconcileAgent(t, reconciler, agent)
	pvc2 := getWorkspacePVC(t, k8sClient, "squad-pvc-def", agent.Name)
	if pvc2.UID != pvc.UID {
		t.Fatalf("PVC was recreated on a later reconcile (uid %q != %q)", pvc2.UID, pvc.UID)
	}
}

func TestWorkspaceStorageClassSetWhenProvided(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-pvc-sc", "aaaa0002-0000-0000-0000-000000000002", "squad-pvc-sc",
		&skquadv1.AgentStorage{Enabled: true, Size: "5Gi", StorageClass: "fast-ssd", MountPath: "/data"})
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	reconcileAgent(t, reconciler, agent)
	reconcileAgent(t, reconciler, agent)

	pvc := getWorkspacePVC(t, k8sClient, "squad-pvc-sc", agent.Name)
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-ssd" {
		t.Fatalf("storageClassName = %v, want fast-ssd when provided", pvc.Spec.StorageClassName)
	}
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "5Gi" {
		t.Fatalf("PVC size = %q, want 5Gi", got)
	}
}

func TestWorkspaceVolumeMountAndRecreateStrategy(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-pvc-mount", "aaaa0003-0000-0000-0000-000000000003", "squad-pvc-mount",
		&skquadv1.AgentStorage{Enabled: true, MountPath: "/data"})
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent, s104Secret("squad-pvc-mount", agentCredS104), s104Secret("squad-pvc-mount", agentVKeyS104)).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	reconcileAgent(t, reconciler, agent)
	reconcileAgent(t, reconciler, agent)

	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squad.Spec.Namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("deployment strategy = %q, want Recreate (RWO race safety, S-135)", deployment.Spec.Strategy.Type)
	}
	var wsVolume *corev1.Volume
	for i := range deployment.Spec.Template.Spec.Volumes {
		if deployment.Spec.Template.Spec.Volumes[i].Name == workspaceVolumeName {
			wsVolume = &deployment.Spec.Template.Spec.Volumes[i]
		}
	}
	if wsVolume == nil || wsVolume.PersistentVolumeClaim == nil {
		t.Fatalf("workspace PVC volume missing: %#v", deployment.Spec.Template.Spec.Volumes)
	}
	if wsVolume.PersistentVolumeClaim.ClaimName != "agent-"+agent.Name+"-workspace" {
		t.Fatalf("claim name = %q, want agent-%s-workspace", wsVolume.PersistentVolumeClaim.ClaimName, agent.Name)
	}
	mounts := deployment.Spec.Template.Spec.Containers[0].VolumeMounts
	var wsMount *corev1.VolumeMount
	for i := range mounts {
		if mounts[i].Name == workspaceVolumeName {
			wsMount = &mounts[i]
		}
	}
	if wsMount == nil || wsMount.MountPath != "/data" {
		t.Fatalf("workspace mount = %#v, want mountPath /data", wsMount)
	}
	if wsMount != nil && wsMount.ReadOnly {
		t.Fatal("workspace mount must be read-write")
	}
	// Existing Secret mounts must be untouched (credential + virtual-key).
	secretMounts := 0
	for _, m := range mounts {
		if m.Name == volumeAgentCredential || m.Name == volumeAgentVirtualKey {
			secretMounts++
		}
	}
	if secretMounts != 2 {
		t.Fatalf("secret mounts = %d, want 2 (unchanged by S-135)", secretMounts)
	}
}

func TestWorkspaceAbsentWhenStorageDisabled(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-pvc-off", "aaaa0004-0000-0000-0000-000000000004", "squad-pvc-off", nil)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent, s104Secret("squad-pvc-off", agentCredS104), s104Secret("squad-pvc-off", agentVKeyS104)).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	reconcileAgent(t, reconciler, agent)
	reconcileAgent(t, reconciler, agent)

	var pvc corev1.PersistentVolumeClaim
	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: squad.Spec.Namespace, Name: "agent-" + agent.Name + "-workspace"}, &pvc)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("PVC must not exist when storage disabled, got err=%v", err)
	}
	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squad.Spec.Namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	for _, v := range deployment.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Fatalf("unexpected PVC volume when storage disabled: %#v", v)
		}
	}
	// No WorkspaceReady condition when storage is not configured.
	if cond := getCondition(t, k8sClient, types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}, "WorkspaceReady"); cond != nil {
		t.Fatalf("WorkspaceReady condition present without storage: %#v", cond)
	}
}

func TestWorkspacePendingBlocksAgentReadiness(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-pvc-pending", "aaaa0005-0000-0000-0000-000000000005", "squad-pvc-pending",
		&skquadv1.AgentStorage{Enabled: true})
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent, s104Secret("squad-pvc-pending", agentCredS104), s104Secret("squad-pvc-pending", agentVKeyS104)).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}
	agentKey := types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}

	// Create the deployment and mark it ready — but the PVC is still
	// Pending. The agent must NOT be Ready: no silent emptyDir fallback.
	reconcileAgent(t, reconciler, agent)
	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squad.Spec.Namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ReadyReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := k8sClient.Status().Update(context.Background(), &deployment); err != nil {
		t.Fatal(err)
	}

	result := reconcileAgent(t, reconciler, agent)
	got := getAgentStatus(t, k8sClient, agentKey)
	if got.Status.Ready {
		t.Fatalf("agent must NOT be Ready while workspace PVC is Pending (reason=%q)", got.Status.Reason)
	}
	if got.Status.Reason != "WorkspacePending" {
		t.Fatalf("reason = %q, want WorkspacePending", got.Status.Reason)
	}
	if cond := getCondition(t, k8sClient, agentKey, "WorkspaceReady"); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Pending" {
		t.Fatalf("WorkspaceReady condition = %#v, want False/Pending", cond)
	}
	if result.RequeueAfter != readinessRequeue {
		t.Fatalf("requeueAfter = %v, want %v while workspace pending", result.RequeueAfter, readinessRequeue)
	}

	// PVC binds → WorkspaceReady True and (pod ready) agent Ready.
	pvc := getWorkspacePVC(t, k8sClient, squad.Spec.Namespace, agent.Name)
	pvc.Status.Phase = corev1.ClaimBound
	if err := k8sClient.Status().Update(context.Background(), &pvc); err != nil {
		t.Fatal(err)
	}
	reconcileAgent(t, reconciler, agent)
	if cond := getCondition(t, k8sClient, agentKey, "WorkspaceReady"); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Bound" {
		t.Fatalf("WorkspaceReady condition = %#v, want True/Bound", cond)
	}
	if got := getAgentStatus(t, k8sClient, agentKey); !got.Status.Ready {
		t.Fatalf("agent must be Ready once workspace bound and pod ready (reason=%q)", got.Status.Reason)
	}
}

func TestWorkspaceInvalidSizeSurfacesCondition(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad, agent := pvcAgent("agent-pvc-bad", "aaaa0006-0000-0000-0000-000000000006", "squad-pvc-bad",
		&skquadv1.AgentStorage{Enabled: true, Size: "not-a-quantity"})
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	reconcileAgent(t, reconciler, agent)
	agentKey := types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}
	if cond := getCondition(t, k8sClient, agentKey, "WorkspaceReady"); cond == nil || cond.Reason != "InvalidStorageSize" || cond.Status != metav1.ConditionFalse {
		t.Fatalf("WorkspaceReady condition = %#v, want False/InvalidStorageSize", cond)
	}
	if got := getAgentStatus(t, k8sClient, agentKey); got.Status.Ready {
		t.Fatal("agent must not be Ready with invalid storage size")
	}
}

func TestWorkspaceDeletedWithAgentUnlessRetained(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		annotation map[string]string
		wantGone   bool
	}{
		{"cleanup", nil, true},
		{"retain", map[string]string{RetainPVCKeepAnnotation: "true"}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scheme := testScheme(t)
			ns := "squad-pvc-del-" + tc.name
			squad, agent := pvcAgent("agent-pvc-del-"+tc.name, "aaaa0007-0000-0000-0000-00000000000"+map[string]string{"cleanup": "7", "retain": "8"}[tc.name], ns,
				&skquadv1.AgentStorage{Enabled: true})
			agent.Annotations = tc.annotation
			pvc := boundPVC(ns, agent.Name)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: ns},
			}
			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
				WithObjects(squad, agent, pvc, deployment).
				Build()
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

			if err := k8sClient.Delete(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			reconcileAgent(t, reconciler, agent)

			err := k8sClient.Get(context.Background(), client.ObjectKey{Name: pvc.Name, Namespace: ns}, &corev1.PersistentVolumeClaim{})
			if tc.wantGone && !apierrors.IsNotFound(err) {
				t.Fatalf("workspace PVC still exists after agent delete: %v", err)
			}
			if !tc.wantGone && err != nil {
				t.Fatalf("retained PVC was deleted: %v", err)
			}
			err = k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: ns}, &appsv1.Deployment{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("deployment should always be cleaned up, got %v", err)
			}
		})
	}
}

func TestWorkspaceConfigDefaults(t *testing.T) {
	t.Parallel()

	if cfg := workspaceConfig(&skquadv1.Agent{}); cfg != nil {
		t.Fatalf("nil storage must yield nil config, got %#v", cfg)
	}
	if cfg := workspaceConfig(&skquadv1.Agent{Spec: skquadv1.AgentSpec{Storage: &skquadv1.AgentStorage{Enabled: false}}}); cfg != nil {
		t.Fatalf("disabled storage must yield nil config, got %#v", cfg)
	}
	cfg := workspaceConfig(&skquadv1.Agent{Spec: skquadv1.AgentSpec{Storage: &skquadv1.AgentStorage{Enabled: true}}})
	if cfg == nil || cfg.Size != defaultWorkspaceSize || cfg.MountPath != defaultWorkspaceMountPath || cfg.StorageClass != "" {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
}
