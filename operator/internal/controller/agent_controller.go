package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

const (
	agentContainerName = "agent"
	agentFinalizer     = "skquad.io/agent-cleanup"
	defaultAgentImage  = "skquad/agent-runtime:0.1.0"
	credentialsMount   = "/var/run/skquad/credentials" // #nosec G101 -- mount path, not a credential
	workspacesMount    = "/var/run/skquad/workspaces"
	runtimeHTTPPort    = int32(8080)
	// Volume names for the credential / virtual-key Secret mounts
	// (S-126 / S1192: single source of truth).
	volumeAgentCredential = "agent-credential" // #nosec G101 -- Kubernetes volume name, not a credential value.
	volumeAgentVirtualKey = "agent-virtual-key"
	// LabelAgentID links a Deployment back to its Agent identity.
	LabelAgentID = "skquad.io/agent-id"
	// readinessRequeue keeps the operator re-checking an agent whose pod is
	// not ready yet (S-104). Without it a DesiredActive agent that never
	// becomes ready is never revisited.
	readinessRequeue = 10 * time.Second
)

// AgentReconciler reconciles Agent resources into per-agent Deployments.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile ensures the agent Deployment exists in its squad namespace.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var agent skquadv1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !agent.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &agent)
	}
	if controllerutil.AddFinalizer(&agent, agentFinalizer) {
		if err := r.Update(ctx, &agent); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	namespace, err := r.squadNamespaceForAgent(ctx, &agent)
	if err != nil {
		return ctrl.Result{}, err
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: namespace},
	}
	replicas := desiredReplicas(&agent, deployment, time.Now)
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		// Recompute inside the closure: CreateOrUpdate fetches the current
		// Deployment first, and desiredReplicas depends on its current
		// spec.replicas (idle scale-down logic).
		replicas = r.applyAgentDeploymentSpec(&agent, deployment)
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// S-104: Ready must derive from observed cluster state (secrets present,
	// mounts applied, pod actually ready) — never from "CR write succeeded".
	ready, reason, message := r.evaluateAgentReadiness(ctx, &agent, deployment, namespace, replicas)

	return r.updateAgentStatus(ctx, &agent, deployment, ready, reason, message, replicas)
}

// reconcileDelete handles an Agent that is being deleted: it cleans up the
// managed resources and removes the finalizer.
func (r *AgentReconciler) reconcileDelete(ctx context.Context, agent *skquadv1.Agent) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(agent, agentFinalizer) {
		if err := r.cleanupAgent(ctx, agent); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(agent, agentFinalizer)
		if err := r.Update(ctx, agent); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// applyAgentDeploymentSpec sets the desired Deployment state for the agent
// and returns the desired replica count computed against the Deployment's
// current state.
func (r *AgentReconciler) applyAgentDeploymentSpec(agent *skquadv1.Agent, deployment *appsv1.Deployment) int32 {
	replicas := desiredReplicas(agent, deployment, time.Now)
	labels := agentLabels(agent)
	ensureAgentLabels(&deployment.Labels, agent)
	deployment.Spec.Replicas = &replicas
	deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	deployment.Spec.Template.ObjectMeta.Labels = labels
	deployment.Spec.Template.Spec.ServiceAccountName = agentServiceAccountName
	// The agent runtime never talks to the Kubernetes API; credentials
	// arrive via projected Secret volumes. Keep the (permissionless) SA
	// token out of the pod entirely and harden the container.
	deployment.Spec.Template.Spec.AutomountServiceAccountToken = boolPtr(false)
	container := corev1.Container{
		Name:  agentContainerName,
		Image: agentImage(agent),
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: runtimeHTTPPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env:            agentEnv(agent),
		LivenessProbe:  httpProbe("/healthz"),
		ReadinessProbe: httpProbe("/readyz"),
		Resources:      agentResourceRequirements(),
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             boolPtr(true),
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
	volumes := agentSecretVolumes(agent)
	if len(volumes) > 0 {
		container.VolumeMounts = agentSecretVolumeMounts(agent)
		deployment.Spec.Template.Spec.Volumes = volumes
	} else {
		deployment.Spec.Template.Spec.Volumes = nil
	}
	deployment.Spec.Template.Spec.Containers = []corev1.Container{container}
	return replicas
}

// agentEnv builds the container environment for the agent runtime.
func agentEnv(agent *skquadv1.Agent) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "SKQUAD_AGENT_ID", Value: agent.Spec.AgentID},
		{Name: "SKQUAD_SQUAD_ID", Value: agent.Spec.SquadID},
		{Name: "SKQUAD_AGENT_ROLE", Value: agent.Spec.Role},
		{Name: "SKQUAD_AGENT_SYSTEM_PROMPT", Value: agent.Spec.SystemPrompt},
		// WP8 step-4 cutover: SKQUAD_DEFAULT_PROVIDER_ID is gone — the
		// legacy provider-uuid env is no longer injected. SKQUAD_DEFAULT_MODEL
		// now carries ONLY the resolved bound AI Model name (control-plane
		// CR writer); unbound agents get an empty value and the runtime
		// fails loudly instead of serving stale legacy config.
		{Name: "SKQUAD_DEFAULT_MODEL", Value: agent.Spec.DefaultModel},
		// WP5 (ADR-0010): the binding itself, so the runtime and any
		// in-pod tooling can see which AI Model the agent is bound to
		// and which fallback the gateway may serve. SKQUAD_DEFAULT_MODEL
		// above stays populated from the AI Model's model_name (via the
		// control-plane CR writer) so the runtime resolves the bound
		// model without changes.
		{Name: "SKQUAD_AI_MODEL_ID", Value: agent.Spec.AIModelID},
		{Name: "SKQUAD_FALLBACK_MODEL_ID", Value: agent.Spec.FallbackAIModelID},
		{Name: "SKQUAD_IDLE_TIMEOUT", Value: agent.Spec.IdleTimeout},
		{Name: "SKQUAD_RUNTIME_PORT", Value: fmt.Sprintf("%d", runtimeHTTPPort)},
		{Name: "SKQUAD_CREDENTIALS_DIR", Value: credentialsMount},
		{Name: "SKQUAD_AGENT_CREDENTIAL_PATH", Value: credentialsMount + "/agent"},
		{Name: "SKQUAD_LLM_GATEWAY_VIRTUAL_KEY_PATH", Value: credentialsMount + "/llm-gateway"},
		{Name: "SKQUAD_CONTROL_PLANE_URL", Value: agent.Spec.ControlPlaneURL},
		{Name: "SKQUAD_LLM_GATEWAY_URL", Value: agent.Spec.LLMGatewayURL},
		{Name: "SKQUAD_TASK_LOOP_ENABLED", Value: "true"},
		{Name: "SKQUAD_TASK_POLL_INTERVAL_SECONDS", Value: envOrDefault("SKQUAD_AGENT_TASK_POLL_INTERVAL_SECONDS", "30")},
		{Name: "SKQUAD_INBOX_POLL_INTERVAL_SECONDS", Value: envOrDefault("SKQUAD_AGENT_INBOX_POLL_INTERVAL_SECONDS", "30")},
		{Name: "SKQUAD_INBOX_BATCH_SIZE", Value: envOrDefault("SKQUAD_AGENT_INBOX_BATCH_SIZE", "5")},
		{Name: "SKQUAD_TASK_TIMEOUT_SECONDS", Value: envOrDefault("SKQUAD_AGENT_TASK_TIMEOUT_SECONDS", "900")},
		{Name: "SKQUAD_MAX_LLM_STEPS", Value: envOrDefault("SKQUAD_AGENT_MAX_LLM_STEPS", "8")},
		{Name: "SKQUAD_TASK_SUMMARY_MAX_CHARS", Value: envOrDefault("SKQUAD_AGENT_TASK_SUMMARY_MAX_CHARS", "4000")},
	}
}

// updateAgentStatus persists the derived readiness state and chooses the
// next requeue behaviour.
func (r *AgentReconciler) updateAgentStatus(ctx context.Context, agent *skquadv1.Agent, deployment *appsv1.Deployment, ready bool, reason, message string, replicas int32) (ctrl.Result, error) {
	agent.Status.ReadyDeployment = deployment.Name
	agent.Status.Replicas = replicas
	agent.Status.Ready = ready
	if ready {
		agent.Status.Phase = "Ready"
	} else {
		agent.Status.Phase = "Progressing"
	}
	agent.Status.Reason = reason
	updateIdleSince(agent, time.Now)
	agent.Status.UpdatedAt = metav1.Now()
	conditionStatus := metav1.ConditionFalse
	if ready {
		conditionStatus = metav1.ConditionTrue
	}
	setCondition(&agent.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: agent.Generation,
	})
	if err := r.Status().Update(ctx, agent); err != nil {
		if apierrors.IsConflict(err) {
			// Lost a status race; re-check shortly instead of stalling.
			return ctrl.Result{RequeueAfter: readinessRequeue}, nil
		}
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	if !ready {
		// Keep polling so credential-mount/secret drift and never-ready pods
		// are retried instead of being silently accepted (S-104).
		return ctrl.Result{RequeueAfter: readinessRequeue}, nil
	}
	return idleRequeue(agent, replicas, time.Now), nil
}

// evaluateAgentReadiness derives the agent's Ready condition from real cluster
// state (S-104): the referenced credential/virtual-key Secrets must exist, the
// Deployment template must actually mount them, and the Deployment must report
// ready replicas. A scaled-to-zero agent is Ready by definition (nothing should
// run); anything else is only Ready when observed ready.
func (r *AgentReconciler) evaluateAgentReadiness(ctx context.Context, agent *skquadv1.Agent, deployment *appsv1.Deployment, namespace string, replicas int32) (bool, string, string) {
	if replicas == 0 {
		return true, "ScaledToZero", fmt.Sprintf("Deployment %s/%s is scaled to zero", namespace, deployment.Name)
	}
	if agent.Spec.CredentialSecret == "" {
		// The runtime can never pass /readyz without credentials; say so
		// explicitly instead of blaming the pod (S-104).
		return false, "CredentialNotProvisioned", fmt.Sprintf("agent %s/%s has no credential secret reference; provision its identity", agent.Namespace, agent.Name)
	}
	if agent.Spec.CredentialSecret != "" {
		check := secretMountCheck{
			secretName:    agent.Spec.CredentialSecret,
			volumeName:    volumeAgentCredential,
			label:         "credential",
			missingReason: "CredentialSecretMissing",
			mountReason:   "CredentialMountMissing",
		}
		if ok, reason, msg := r.checkSecretAndMount(ctx, deployment, namespace, check); !ok {
			return false, reason, msg
		}
	}
	if agent.Spec.VirtualKeySecret != "" {
		check := secretMountCheck{
			secretName:    agent.Spec.VirtualKeySecret,
			volumeName:    volumeAgentVirtualKey,
			label:         "virtual-key",
			missingReason: "VirtualKeySecretMissing",
			mountReason:   "VirtualKeyMountMissing",
		}
		if ok, reason, msg := r.checkSecretAndMount(ctx, deployment, namespace, check); !ok {
			return false, reason, msg
		}
	}
	if deployment.Status.ReadyReplicas < replicas {
		return false, "PodNotReady", fmt.Sprintf("deployment %s/%s: %d/%d replicas ready, %d unavailable", namespace, deployment.Name, deployment.Status.ReadyReplicas, replicas, deployment.Status.UnavailableReplicas)
	}
	return true, "DeploymentReady", fmt.Sprintf("Deployment %s/%s is ready", namespace, deployment.Name)
}

type secretMountCheck struct {
	secretName    string
	volumeName    string
	label         string
	missingReason string
	mountReason   string
}

// checkSecretAndMount verifies one credential-class Secret exists and the
// Deployment actually mounts it (volume + container mount). Extracted
// from evaluateAgentReadiness for cognitive complexity (S-126 / S3776).
func (r *AgentReconciler) checkSecretAndMount(ctx context.Context, deployment *appsv1.Deployment, namespace string, check secretMountCheck) (bool, string, string) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: check.secretName}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, check.missingReason, fmt.Sprintf("%s secret %s/%s has not been created yet", check.label, namespace, check.secretName)
		}
		return false, "ReadinessCheckFailed", err.Error()
	}
	if !deploymentHasVolume(deployment, check.volumeName) || !containerHasVolumeMount(deployment, check.volumeName) {
		return false, check.mountReason, fmt.Sprintf("deployment %s/%s does not mount %s secret %s", namespace, deployment.Name, check.label, check.secretName)
	}
	return true, "", ""
}

func deploymentHasVolume(deployment *appsv1.Deployment, name string) bool {
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == name {
			return true
		}
	}
	return false
}

func containerHasVolumeMount(deployment *appsv1.Deployment, name string) bool {
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, mount := range container.VolumeMounts {
			if mount.Name == name {
				return true
			}
		}
	}
	return false
}

func (r *AgentReconciler) cleanupAgent(ctx context.Context, agent *skquadv1.Agent) error {
	namespace, err := r.squadNamespaceForAgent(ctx, agent)
	if err != nil {
		return err
	}
	return deleteIfExists(ctx, r.Client, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: namespace},
	})
}

func envOrDefault(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// agentResourceRequirements gives the agent container explicit cpu/memory
// requests+limits. Tenant namespaces carry a ResourceQuota that requires
// every container to declare them (S-85); without this the ReplicaSet is
// rejected at admission and the agent can never wake. Overridable per
// operator via env; invalid values fall back to the defaults.
func agentResourceRequirements() corev1.ResourceRequirements {
	parse := func(value, fallback string) resource.Quantity {
		if q, err := resource.ParseQuantity(value); err == nil {
			return q
		}
		return resource.MustParse(fallback)
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    parse(envOrDefault("SKQUAD_AGENT_CPU_REQUEST", "100m"), "100m"),
			corev1.ResourceMemory: parse(envOrDefault("SKQUAD_AGENT_MEMORY_REQUEST", "128Mi"), "128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    parse(envOrDefault("SKQUAD_AGENT_CPU_LIMIT", "500m"), "500m"),
			corev1.ResourceMemory: parse(envOrDefault("SKQUAD_AGENT_MEMORY_LIMIT", "512Mi"), "512Mi"),
		},
	}
}

// SetupWithManager registers the Agent controller with a controller-runtime
// manager.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// The agent Deployment lives in the squad namespace while the Agent CR
	// lives in the operator namespace, so an ownerRef-based Owns() is not
	// valid cross-namespace. Watch Deployments and map them back to their
	// Agent via the skquad.io/agent-id label — this is the self-healing
	// path for agents whose pods regress or never become ready (S-104).
	return ctrl.NewControllerManagedBy(mgr).
		For(&skquadv1.Agent{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapDeploymentToAgent)).
		Complete(r)
}

// mapDeploymentToAgent maps a Deployment status/spec change to the owning
// Agent CR by the skquad.io/agent-id label the control plane sets on both.
func (r *AgentReconciler) mapDeploymentToAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	agentID := obj.GetLabels()[LabelAgentID]
	if agentID == "" {
		return nil
	}
	var agents skquadv1.AgentList
	if err := r.List(ctx, &agents, client.MatchingLabels{LabelAgentID: agentID}); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(agents.Items))
	for i := range agents.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&agents.Items[i])})
	}
	return requests
}

func (r *AgentReconciler) squadNamespaceForAgent(ctx context.Context, agent *skquadv1.Agent) (string, error) {
	var squads skquadv1.SquadList
	if err := r.List(ctx, &squads, client.InNamespace(agent.Namespace)); err != nil {
		return "", err
	}
	for i := range squads.Items {
		if squads.Items[i].Spec.SquadID == agent.Spec.SquadID {
			return SquadNamespace(&squads.Items[i]), nil
		}
	}
	if agent.Spec.SquadID == "" {
		return "", fmt.Errorf("agent %s/%s has empty squadId", agent.Namespace, agent.Name)
	}
	return "squad-" + agent.Spec.SquadID, nil
}

func agentLabels(agent *skquadv1.Agent) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": managedBy,
		"app.kubernetes.io/name":       "skquad-agent",
		LabelAgentID:                   agent.Spec.AgentID,
		"skquad.io/squad-id":           agent.Spec.SquadID,
	}
}

func ensureAgentLabels(labels *map[string]string, agent *skquadv1.Agent) {
	if *labels == nil {
		*labels = map[string]string{}
	}
	for key, value := range agentLabels(agent) {
		(*labels)[key] = value
	}
}

func agentImage(agent *skquadv1.Agent) string {
	if agent.Spec.Image == "" {
		return defaultAgentImage
	}
	return agent.Spec.Image
}

func desiredReplicas(agent *skquadv1.Agent, deployment *appsv1.Deployment, now func() time.Time) int32 {
	if agent.Spec.DesiredActive {
		return 1
	}
	current := int32(0)
	if deployment.Spec.Replicas != nil {
		current = *deployment.Spec.Replicas
	}
	if current == 0 {
		return 0
	}
	timeout := idleTimeout(agent)
	if timeout <= 0 {
		return 0
	}
	if agent.Status.IdleSince.IsZero() {
		return 1
	}
	if now().Sub(agent.Status.IdleSince.Time) < timeout {
		return 1
	}
	return 0
}

func updateIdleSince(agent *skquadv1.Agent, now func() time.Time) {
	if agent.Spec.DesiredActive {
		agent.Status.IdleSince = metav1.Time{}
		return
	}
	if agent.Status.IdleSince.IsZero() {
		agent.Status.IdleSince = metav1.NewTime(now().UTC())
	}
}

func idleRequeue(agent *skquadv1.Agent, replicas int32, now func() time.Time) ctrl.Result {
	if agent.Spec.DesiredActive || replicas == 0 || agent.Status.IdleSince.IsZero() {
		return ctrl.Result{}
	}
	timeout := idleTimeout(agent)
	if timeout <= 0 {
		return ctrl.Result{}
	}
	remaining := timeout - now().Sub(agent.Status.IdleSince.Time)
	if remaining <= 0 {
		return ctrl.Result{Requeue: true}
	}
	return ctrl.Result{RequeueAfter: remaining}
}

func idleTimeout(agent *skquadv1.Agent) time.Duration {
	if agent.Spec.IdleTimeout == "" {
		return 0
	}
	timeout, err := time.ParseDuration(agent.Spec.IdleTimeout)
	if err != nil {
		return 0
	}
	return timeout
}

func httpProbe(path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromString("http"),
			},
		},
	}
}

func agentSecretVolumes(agent *skquadv1.Agent) []corev1.Volume {
	var volumes []corev1.Volume
	if agent.Spec.CredentialSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: volumeAgentCredential,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: agent.Spec.CredentialSecret,
			}},
		})
	}
	if agent.Spec.VirtualKeySecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: volumeAgentVirtualKey,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: agent.Spec.VirtualKeySecret,
			}},
		})
	}
	for i, ws := range agent.Spec.WorkspaceSecrets {
		if ws.SecretName == "" || ws.ResourceID == "" {
			continue
		}
		volumes = append(volumes, corev1.Volume{
			Name: fmt.Sprintf("workspace-%d", i),
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: ws.SecretName,
			}},
		})
	}
	return volumes
}

func agentSecretVolumeMounts(agent *skquadv1.Agent) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	if agent.Spec.CredentialSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volumeAgentCredential,
			MountPath: credentialsMount + "/agent",
			ReadOnly:  true,
		})
	}
	if agent.Spec.VirtualKeySecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volumeAgentVirtualKey,
			MountPath: credentialsMount + "/llm-gateway",
			ReadOnly:  true,
		})
	}
	for i, ws := range agent.Spec.WorkspaceSecrets {
		if ws.SecretName == "" || ws.ResourceID == "" {
			continue
		}
		mounts = append(mounts, corev1.VolumeMount{
			Name:      fmt.Sprintf("workspace-%d", i),
			MountPath: fmt.Sprintf("%s/%s", workspacesMount, ws.ResourceID),
			ReadOnly:  true,
		})
	}
	return mounts
}
