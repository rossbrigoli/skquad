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
		ObjectMeta: metav1.ObjectMeta{Name: "squad-test", Namespace: "skquad-system"},
		Spec: skquadv1.SquadSpec{
			SquadID:   "11111111-1111-1111-1111-111111111111",
			Namespace: "squad-runtime-test",
		},
	}
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-test", Namespace: "skquad-system"},
		Spec: skquadv1.AgentSpec{
			AgentID:           "22222222-2222-2222-2222-222222222222",
			SquadID:           squad.Spec.SquadID,
			Role:              "worker",
			DefaultProviderID: "provider-id",
			DefaultModel:      "openai/gpt-4o-mini",
			Image:             "example.com/skquad/agent:test",
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
	if got := envValue(container.Env, "SKQUAD_AGENT_CREDENTIAL_PATH"); got != credentialsMount+"/agent" {
		t.Fatalf("credential path env = %q, want %q", got, credentialsMount+"/agent")
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
	if got := envValue(container.Env, "SKQUAD_DEFAULT_PROVIDER_ID"); got != agent.Spec.DefaultProviderID {
		t.Fatalf("default provider env = %q, want %q", got, agent.Spec.DefaultProviderID)
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
	if got := deployment.Spec.Template.Labels["skquad.io/agent-id"]; got != agent.Spec.AgentID {
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
	if got := mounts[0].MountPath; got != credentialsMount+"/agent" {
		t.Fatalf("credential mount path = %q, want %q", got, credentialsMount+"/agent")
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
		ObjectMeta: metav1.ObjectMeta{Name: "agent-zero", Namespace: "skquad-system", Finalizers: []string{agentFinalizer}},
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
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agent.Name, Namespace: "squad-" + agent.Spec.SquadID}, &deployment); err != nil {
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
		ObjectMeta: metav1.ObjectMeta{Name: "agent-idle-wait", Namespace: "skquad-system", Finalizers: []string{agentFinalizer}},
		Spec: skquadv1.AgentSpec{
			AgentID:     "55555555-5555-5555-5555-555555555555",
			SquadID:     "66666666-6666-6666-6666-666666666666",
			IdleTimeout: "5m",
		},
		Status: skquadv1.AgentStatus{IdleSince: idleSince},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: "squad-" + agent.Spec.SquadID},
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
		ObjectMeta: metav1.ObjectMeta{Name: "agent-idle-expired", Namespace: "skquad-system", Finalizers: []string{agentFinalizer}},
		Spec: skquadv1.AgentSpec{
			AgentID:     "77777777-7777-7777-7777-777777777777",
			SquadID:     "88888888-8888-8888-8888-888888888888",
			IdleTimeout: "5m",
		},
		Status: skquadv1.AgentStatus{IdleSince: metav1.NewTime(time.Now().Add(-10 * time.Minute))},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: "squad-" + agent.Spec.SquadID},
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
		ObjectMeta: metav1.ObjectMeta{Name: "squad-delete-agent", Namespace: "skquad-system"},
		Spec: skquadv1.SquadSpec{
			SquadID:   "99999999-9999-9999-9999-999999999999",
			Namespace: "squad-agent-delete-test",
		},
	}
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "agent-delete",
			Namespace:  "skquad-system",
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
		ObjectMeta: metav1.ObjectMeta{Name: "squad-sec", Namespace: "skquad-system"},
		Spec: skquadv1.SquadSpec{
			SquadID:   "88888888-8888-8888-8888-888888888888",
			Namespace: "squad-sec-ns",
		},
	}
	agent := &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-sec", Namespace: "skquad-system"},
		Spec: skquadv1.AgentSpec{
			AgentID:       "99999999-9999-9999-9999-999999999999",
			SquadID:       squad.Spec.SquadID,
			Image:         "example.com/skquad/agent:test",
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
