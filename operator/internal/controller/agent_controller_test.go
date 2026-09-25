package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

const (
	testAgentImageRef = "example.com/skquad/agent:test"
	agentCredS104     = "agent-cred-s104"
	agentVKeyS104     = "agent-vkey-s104"
	credentialSubPath = "/agent"
	squadPrefix       = "squad-"
)

func TestAgentReconcilerCreatesDeployment(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-test", Namespace: testNamespace},
		Spec: skquadv1.SquadSpec{
			SquadID:   "11111111-1111-1111-1111-111111111111",
			Namespace: "squad-runtime-test",
		},
	}
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-test", Namespace: testNamespace},
		Spec: skquadv1.AgentSpec{
			AgentID:          "22222222-2222-2222-2222-222222222222",
			SquadID:          squad.Spec.SquadID,
			Role:             "worker",
			DefaultModel:     "openai/gpt-4o-mini",
			Image:            testAgentImageRef,
			CredentialSecret: "agent-credential",
			VirtualKeySecret: "agent-virtual-key",
			ControlPlaneURL:  "http://skquad-api-server.skquad-system.svc.cluster.local:8080",
			LLMGatewayURL:    "http://skquad-llm-gateway.skquad-system.svc.cluster.local:4000",
			IdleTimeout:      "300s",
			DesiredActive:    true,
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(squad, agent).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Requeue {
		t.Fatalf("first reconcile result = %#v, want requeue after finalizer add", result)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	}); err != nil {
		t.Fatal(err)
	}

	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squad.Spec.Namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("deployment replicas = %v, want 1", deployment.Spec.Replicas)
	}
	if got := deployment.Spec.Template.Spec.ServiceAccountName; got != agentServiceAccountName {
		t.Fatalf("service account = %q, want %q", got, agentServiceAccountName)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != agent.Spec.Image {
		t.Fatalf("container image = %q, want %q", got, agent.Spec.Image)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if got := container.Ports[0].ContainerPort; got != runtimeHTTPPort {
		t.Fatalf("runtime port = %d, want %d", got, runtimeHTTPPort)
	}
	if container.LivenessProbe == nil || container.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("liveness probe = %#v, want /healthz", container.LivenessProbe)
	}
	if container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("readiness probe = %#v, want /readyz", container.ReadinessProbe)
	}
	if got := envValue(container.Env, "SKQUAD_AGENT_CREDENTIAL_PATH"); got != credentialsMount+credentialSubPath {
		t.Fatalf("credential path env = %q, want %q", got, credentialsMount+credentialSubPath)
	}
	if got := envValue(container.Env, "SKQUAD_LLM_GATEWAY_VIRTUAL_KEY_PATH"); got != credentialsMount+"/llm-gateway" {
		t.Fatalf("virtual key path env = %q, want %q", got, credentialsMount+"/llm-gateway")
	}
	if got := envValue(container.Env, "SKQUAD_CONTROL_PLANE_URL"); got != agent.Spec.ControlPlaneURL {
		t.Fatalf("control plane url env = %q, want %q", got, agent.Spec.ControlPlaneURL)
	}
	if got := envValue(container.Env, "SKQUAD_LLM_GATEWAY_URL"); got != agent.Spec.LLMGatewayURL {
		t.Fatalf("llm gateway url env = %q, want %q", got, agent.Spec.LLMGatewayURL)
	}
	// WP8 step-4 cutover: the legacy SKQUAD_DEFAULT_PROVIDER_ID env is gone.
	for _, ev := range container.Env {
		if ev.Name == "SKQUAD_DEFAULT_PROVIDER_ID" {
			t.Fatalf("SKQUAD_DEFAULT_PROVIDER_ID must not be injected after the WP8 cutover")
		}
	}
	if got := envValue(container.Env, "SKQUAD_DEFAULT_MODEL"); got != agent.Spec.DefaultModel {
		t.Fatalf("default model env = %q, want %q", got, agent.Spec.DefaultModel)
	}
	if got := envValue(container.Env, "SKQUAD_TASK_LOOP_ENABLED"); got != "true" {
		t.Fatalf("task loop enabled env = %q, want true", got)
	}
	if got := envValue(container.Env, "SKQUAD_TASK_POLL_INTERVAL_SECONDS"); got != "30" {
		t.Fatalf("task poll interval env = %q, want 30", got)
	}
	if got := envValue(container.Env, "SKQUAD_INBOX_POLL_INTERVAL_SECONDS"); got != "30" {
		t.Fatalf("inbox poll interval env = %q, want 30", got)
	}
	if got := envValue(container.Env, "SKQUAD_INBOX_BATCH_SIZE"); got != "5" {
		t.Fatalf("inbox batch size env = %q, want 5", got)
	}
	if got := envValue(container.Env, "SKQUAD_TASK_TIMEOUT_SECONDS"); got != "900" {
		t.Fatalf("task timeout env = %q, want 900", got)
	}
	if got := envValue(container.Env, "SKQUAD_MAX_LLM_STEPS"); got != "8" {
		t.Fatalf("max llm steps env = %q, want 8", got)
	}
	if got := envValue(container.Env, "SKQUAD_TASK_SUMMARY_MAX_CHARS"); got != "4000" {
		t.Fatalf("summary max chars env = %q, want 4000", got)
	}
	if got := deployment.Spec.Template.Labels[LabelAgentID]; got != agent.Spec.AgentID {
		t.Fatalf("agent label = %q, want %q", got, agent.Spec.AgentID)
	}
	if got := len(deployment.Spec.Template.Spec.Volumes); got != 2 {
		t.Fatalf("volume count = %d, want 2", got)
	}
	if got := deployment.Spec.Template.Spec.Volumes[0].Secret.SecretName; got != agent.Spec.CredentialSecret {
		t.Fatalf("credential secret = %q, want %q", got, agent.Spec.CredentialSecret)
	}
	if got := deployment.Spec.Template.Spec.Volumes[1].Secret.SecretName; got != agent.Spec.VirtualKeySecret {
		t.Fatalf("virtual key secret = %q, want %q", got, agent.Spec.VirtualKeySecret)
	}
	mounts := deployment.Spec.Template.Spec.Containers[0].VolumeMounts
	if got := len(mounts); got != 2 {
		t.Fatalf("volume mount count = %d, want 2", got)
	}
	if got := mounts[0].MountPath; got != credentialsMount+credentialSubPath {
		t.Fatalf("credential mount path = %q, want %q", got, credentialsMount+credentialSubPath)
	}
	if !mounts[0].ReadOnly || !mounts[1].ReadOnly {
		t.Fatal("secret mounts must be read-only")
	}

	var updatedAgent skquadv1.Agent
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: agent.Namespace}, &updatedAgent); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&updatedAgent, agentFinalizer) {
		t.Fatalf("agent finalizers = %#v, want %q", updatedAgent.Finalizers, agentFinalizer)
	}
}

func TestAgentReconcilerScalesInactiveAgentToZero(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-zero", Namespace: testNamespace, Finalizers: []string{agentFinalizer}},
		Spec: skquadv1.AgentSpec{
			AgentID: "33333333-3333-3333-3333-333333333333",
			SquadID: "44444444-4444-4444-4444-444444444444",
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	}); err != nil {
		t.Fatal(err)
	}

	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squadPrefix + agent.Spec.SquadID}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		t.Fatalf("deployment replicas = %v, want 0", deployment.Spec.Replicas)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != defaultAgentImage {
		t.Fatalf("default image = %q, want %q", got, defaultAgentImage)
	}
}

func TestAgentReconcilerWaitsForIdleTimeoutBeforeScaleDown(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	replicas := int32(1)
	idleSince := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-idle-wait", Namespace: testNamespace, Finalizers: []string{agentFinalizer}},
		Spec: skquadv1.AgentSpec{
			AgentID:     "55555555-5555-5555-5555-555555555555",
			SquadID:     "66666666-6666-6666-6666-666666666666",
			IdleTimeout: "5m",
		},
		Status: skquadv1.AgentStatus{IdleSince: idleSince},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: squadPrefix + agent.Spec.SquadID},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent, deployment).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	})
	if err != nil {
		t.Fatal(err)
	}

	var updated appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: deployment.Namespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != 1 {
		t.Fatalf("deployment replicas = %v, want 1 during idle timeout", updated.Spec.Replicas)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("requeueAfter = %v, want positive idle timeout wait", result.RequeueAfter)
	}
}

func TestAgentReconcilerScalesDownAfterIdleTimeout(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	replicas := int32(1)
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-idle-expired", Namespace: testNamespace, Finalizers: []string{agentFinalizer}},
		Spec: skquadv1.AgentSpec{
			AgentID:     "77777777-7777-7777-7777-777777777777",
			SquadID:     "88888888-8888-8888-8888-888888888888",
			IdleTimeout: "5m",
		},
		Status: skquadv1.AgentStatus{IdleSince: metav1.NewTime(time.Now().Add(-10 * time.Minute))},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: squadPrefix + agent.Spec.SquadID},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent, deployment).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	})
	if err != nil {
		t.Fatal(err)
	}

	var updated appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: deployment.Namespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != 0 {
		t.Fatalf("deployment replicas = %v, want 0 after idle timeout", updated.Spec.Replicas)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("result = %#v, want no requeue after scale down", result)
	}
}

func TestAgentReconcilerFinalizerDeletesDeployment(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-delete-agent", Namespace: testNamespace},
		Spec: skquadv1.SquadSpec{
			SquadID:   "99999999-9999-9999-9999-999999999999",
			Namespace: "squad-agent-delete-test",
		},
	}
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "agent-delete",
			Namespace:  testNamespace,
			Finalizers: []string{agentFinalizer},
		},
		Spec: skquadv1.AgentSpec{
			AgentID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			SquadID: squad.Spec.SquadID,
		},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: squad.Spec.Namespace},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(squad, agent, deployment).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	if err := k8sClient.Delete(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	}); err != nil {
		t.Fatal(err)
	}

	var deleted appsv1.Deployment
	err := k8sClient.Get(context.Background(), client.ObjectKey{Name: deployment.Name, Namespace: deployment.Namespace}, &deleted)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("deployment still exists or lookup failed: %v", err)
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name {
			return item.Value
		}
	}
	return ""
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestAgentReconcilerHardensPodSecurity(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-sec", Namespace: testNamespace},
		Spec: skquadv1.SquadSpec{
			SquadID:   "88888888-8888-8888-8888-888888888888",
			Namespace: "squad-sec-ns",
		},
	}
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-sec", Namespace: testNamespace},
		Spec: skquadv1.AgentSpec{
			AgentID:       "99999999-9999-9999-9999-999999999999",
			SquadID:       squad.Spec.SquadID,
			Image:         testAgentImageRef,
			IdleTimeout:   "300s",
			DesiredActive: true,
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(squad, agent).Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squad.Spec.Namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Spec.AutomountServiceAccountToken == nil || *deployment.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("pod automountServiceAccountToken = %v, want false", deployment.Spec.Template.Spec.AutomountServiceAccountToken)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	sc := container.SecurityContext
	if sc == nil {
		t.Fatal("agent container has no securityContext")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatal("runAsNonRoot = false, want true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatal("allowPrivilegeEscalation = true, want false")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capabilities = %#v, want drop ALL", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("seccompProfile = %#v, want RuntimeDefault", sc.SeccompProfile)
	}
}

// --- S-104: Ready must derive from real cluster state, not CR write success ---

func s104Agent(name string, squadID string) *skquadv1.Agent {
	return &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  testNamespace,
			Finalizers: []string{agentFinalizer},
			Labels:     map[string]string{LabelAgentID: "aaaaaaaa-1111-1111-1111-111111111111"},
		},
		Spec: skquadv1.AgentSpec{
			AgentID:          "aaaaaaaa-1111-1111-1111-111111111111",
			SquadID:          squadID,
			Role:             "worker",
			Image:            testAgentImageRef,
			CredentialSecret: agentCredS104,
			VirtualKeySecret: agentVKeyS104,
			IdleTimeout:      "300s",
			DesiredActive:    true,
		},
	}
}

func s104Secret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("c2VjcmV0")},
	}
}

func getAgentStatus(t *testing.T, c client.Client, key types.NamespacedName) skquadv1.Agent {
	t.Helper()
	var agent skquadv1.Agent
	if err := c.Get(context.Background(), key, &agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestAgentReconcilerNotReadyWhenCredentialSecretMissing(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squadNS := "squad-s104-missing"
	agent := s104Agent("agent-s104-missing", "bbbbbbbb-2222-2222-2222-222222222222")
	// Squad exists so namespace resolution works; credential Secret does NOT.
	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-s104-missing", Namespace: testNamespace},
		Spec:       skquadv1.SquadSpec{SquadID: agent.Spec.SquadID, Namespace: squadNS},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).WithObjects(squad, agent).Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != readinessRequeue {
		t.Fatalf("requeueAfter = %v, want %v (must keep retrying)", result.RequeueAfter, readinessRequeue)
	}
	got := getAgentStatus(t, k8sClient, types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace})
	if got.Status.Ready {
		t.Fatalf("agent must NOT be Ready when credential secret is missing (reason=%q)", got.Status.Reason)
	}
	if got.Status.Reason != "CredentialSecretMissing" {
		t.Fatalf("reason = %q, want CredentialSecretMissing", got.Status.Reason)
	}
	if got.Status.Phase != "Progressing" {
		t.Fatalf("phase = %q, want Progressing", got.Status.Phase)
	}
}

func TestAgentReconcilerNotReadyUntilPodReportsReady(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squadNS := "squad-s104-pod"
	agent := s104Agent("agent-s104-pod", "cccccccc-3333-3333-3333-333333333333")
	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-s104-pod", Namespace: testNamespace},
		Spec:       skquadv1.SquadSpec{SquadID: agent.Spec.SquadID, Namespace: squadNS},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent, s104Secret(squadNS, agentCredS104), s104Secret(squadNS, agentVKeyS104)).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	// First reconcile: deployment created, but ReadyReplicas=0 → not ready.
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != readinessRequeue {
		t.Fatalf("requeueAfter = %v, want %v while pod not ready", result.RequeueAfter, readinessRequeue)
	}
	got := getAgentStatus(t, k8sClient, types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace})
	if got.Status.Ready {
		t.Fatal("agent must NOT be Ready when deployment has 0 ready replicas")
	}
	if got.Status.Reason != "PodNotReady" {
		t.Fatalf("reason = %q, want PodNotReady", got.Status.Reason)
	}

	// Simulate the pod becoming ready.
	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squadNS}, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ReadyReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := k8sClient.Status().Update(context.Background(), &deployment); err != nil {
		t.Fatal(err)
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	}); err != nil {
		t.Fatal(err)
	}
	got = getAgentStatus(t, k8sClient, types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace})
	if !got.Status.Ready {
		t.Fatalf("agent must be Ready once pod is ready (reason=%q)", got.Status.Reason)
	}
	if got.Status.Reason != "DeploymentReady" || got.Status.Phase != "Ready" {
		t.Fatalf("reason/phase = %q/%q, want DeploymentReady/Ready", got.Status.Reason, got.Status.Phase)
	}
}

func TestAgentReconcilerSelfHealsReadyFlagWhenPodRegresses(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	squadNS := "squad-s104-heal"
	agent := s104Agent("agent-s104-heal", "dddddddd-4444-4444-4444-444444444444")
	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-s104-heal", Namespace: testNamespace},
		Spec:       skquadv1.SquadSpec{SquadID: agent.Spec.SquadID, Namespace: squadNS},
	}
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agent.Name,
			Namespace: squadNS,
			Labels:    map[string]string{LabelAgentID: agent.Spec.AgentID},
		},
		Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent, deployment, s104Secret(squadNS, agentCredS104), s104Secret(squadNS, agentVKeyS104)).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	// Reconcile with a ready deployment, but the deployment template has no
	// credential mounts yet (CreateOrUpdate will add them). After the first
	// pass the mounts exist; verify ready, then regress the pod.
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	}); err != nil {
		t.Fatal(err)
	}
	// Second pass after mounts applied: should be Ready.
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	}); err != nil {
		t.Fatal(err)
	}
	agentKey := types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}
	if got := getAgentStatus(t, k8sClient, agentKey); !got.Status.Ready {
		t.Fatalf("agent should be Ready with ready pod + mounts (reason=%q)", got.Status.Reason)
	}

	// Pod regresses to 0 ready replicas (e.g. crash-loop after node loss).
	var dep appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squadNS}, &dep); err != nil {
		t.Fatal(err)
	}
	dep.Status.ReadyReplicas = 0
	dep.Status.AvailableReplicas = 0
	dep.Status.UnavailableReplicas = 1
	if err := k8sClient.Status().Update(context.Background(), &dep); err != nil {
		t.Fatal(err)
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: agentKey})
	if err != nil {
		t.Fatal(err)
	}
	got := getAgentStatus(t, k8sClient, agentKey)
	if got.Status.Ready {
		t.Fatal("agent must lose Ready when pod regresses to 0 ready replicas")
	}
	if got.Status.Reason != "PodNotReady" {
		t.Fatalf("reason = %q, want PodNotReady", got.Status.Reason)
	}
	if result.RequeueAfter != readinessRequeue {
		t.Fatalf("requeueAfter = %v, want %v to keep self-healing", result.RequeueAfter, readinessRequeue)
	}
}

func TestMapDeploymentToAgent(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	agent := s104Agent("agent-s104-map", "eeeeeeee-5555-5555-5555-555555555555")
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-s104-map",
			Namespace: "squad-eeeeeeee-5555-5555-5555-555555555555",
			Labels:    map[string]string{LabelAgentID: agent.Spec.AgentID},
		},
	}
	requests := reconciler.mapDeploymentToAgent(context.Background(), dep)
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if requests[0].NamespacedName.Name != agent.Name || requests[0].NamespacedName.Namespace != agent.Namespace {
		t.Fatalf("mapped request = %v, want %s/%s", requests[0].NamespacedName, agent.Namespace, agent.Name)
	}

	// Deployment without the agent-id label maps to nothing.
	orphan := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "default"}}
	if got := reconciler.mapDeploymentToAgent(context.Background(), orphan); len(got) != 0 {
		t.Fatalf("unlabeled deployment mapped to %d requests, want 0", len(got))
	}
}

func TestAgentReconcilerFlagsCredentialNotProvisioned(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	agent := s104Agent("agent-s104-noid", "ffffffff-6666-6666-6666-666666666666")
	agent.Spec.CredentialSecret = ""
	agent.Spec.VirtualKeySecret = ""
	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-s104-noid", Namespace: testNamespace},
		Spec:       skquadv1.SquadSpec{SquadID: agent.Spec.SquadID, Namespace: "squad-s104-noid"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&skquadv1.Agent{}, &skquadv1.Squad{}).
		WithObjects(squad, agent).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
	}); err != nil {
		t.Fatal(err)
	}
	got := getAgentStatus(t, k8sClient, types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace})
	if got.Status.Ready {
		t.Fatal("agent without credential secret must not be Ready")
	}
	if got.Status.Reason != "CredentialNotProvisioned" {
		t.Fatalf("reason = %q, want CredentialNotProvisioned", got.Status.Reason)
	}
}

// WP5 (ADR-0010 D4): the operator injects the model binding env vars
// and keeps SKQUAD_DEFAULT_MODEL populated from the AI Model's
// model_name (carried on the CR by the control-plane writer).
func TestAgentDeploymentInjectsModelBindingEnv(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "squad-bind", Namespace: testNamespace},
		Spec: skquadv1.SquadSpec{
			SquadID:   "33333333-3333-3333-3333-333333333333",
			Namespace: "squad-bind",
		},
	}
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-bind", Namespace: testNamespace},
		Spec: skquadv1.AgentSpec{
			AgentID:           "44444444-4444-4444-4444-444444444444",
			SquadID:           squad.Spec.SquadID,
			Role:              "worker",
			DefaultModel:      "gpt-6-sol",
			AIModelID:         "ai-primary-uuid",
			FallbackAIModelID: "ai-fallback-uuid",
			Image:             testAgentImageRef,
			CredentialSecret:  "agent-credential",
			VirtualKeySecret:  "agent-virtual-key",
			ControlPlaneURL:   "http://skquad-api-server.skquad-system.svc.cluster.local:8080",
			LLMGatewayURL:     "http://skquad-llm-gateway.skquad-system.svc.cluster.local:4000",
			IdleTimeout:       "300s",
			DesiredActive:     true,
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(squad, agent).
		Build()
	reconciler := &AgentReconciler{Client: k8sClient, Scheme: scheme}

	// First pass adds the finalizer and requeues; second pass creates the
	// Deployment (same two-pass pattern as the other tests here).
	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace},
		}); err != nil {
			t.Fatalf("reconcile pass %d: %v", i, err)
		}
	}

	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: squad.Spec.Namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if got := envValue(container.Env, "SKQUAD_AI_MODEL_ID"); got != "ai-primary-uuid" {
		t.Fatalf("SKQUAD_AI_MODEL_ID = %q, want ai-primary-uuid", got)
	}
	if got := envValue(container.Env, "SKQUAD_FALLBACK_MODEL_ID"); got != "ai-fallback-uuid" {
		t.Fatalf("SKQUAD_FALLBACK_MODEL_ID = %q, want ai-fallback-uuid", got)
	}
	// Runtime compatibility: SKQUAD_DEFAULT_MODEL still carries the AI
	// Model's model_name so the existing resolution keeps working.
	if got := envValue(container.Env, "SKQUAD_DEFAULT_MODEL"); got != "gpt-6-sol" {
		t.Fatalf("SKQUAD_DEFAULT_MODEL = %q, want gpt-6-sol", got)
	}
}
