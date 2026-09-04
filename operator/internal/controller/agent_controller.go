package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

const (
	agentContainerName = "agent"
	agentFinalizer     = "skquad.io/agent-cleanup"
	defaultAgentImage  = "skquad/agent-runtime:0.1.0"
	credentialsMount   = "/var/run/skquad/credentials"
	runtimeHTTPPort    = int32(8080)
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

	if agent.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.AddFinalizer(&agent, agentFinalizer) {
			if err := r.Update(ctx, &agent); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		if controllerutil.ContainsFinalizer(&agent, agentFinalizer) {
			if err := r.cleanupAgent(ctx, &agent); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&agent, agentFinalizer)
			if err := r.Update(ctx, &agent); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
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
		replicas = desiredReplicas(&agent, deployment, time.Now)
		labels := agentLabels(&agent)
		ensureAgentLabels(&deployment.Labels, &agent)
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deployment.Spec.Template.ObjectMeta.Labels = labels
		deployment.Spec.Template.Spec.ServiceAccountName = agentServiceAccountName
		container := corev1.Container{
			Name:  agentContainerName,
			Image: agentImage(&agent),
			Ports: []corev1.ContainerPort{{
				Name:          "http",
				ContainerPort: runtimeHTTPPort,
				Protocol:      corev1.ProtocolTCP,
			}},
			Env: []corev1.EnvVar{
				{Name: "SKQUAD_AGENT_ID", Value: agent.Spec.AgentID},
				{Name: "SKQUAD_SQUAD_ID", Value: agent.Spec.SquadID},
				{Name: "SKQUAD_AGENT_ROLE", Value: agent.Spec.Role},
				{Name: "SKQUAD_AGENT_SYSTEM_PROMPT", Value: agent.Spec.SystemPrompt},
				{Name: "SKQUAD_DEFAULT_PROVIDER_ID", Value: agent.Spec.DefaultProviderID},
				{Name: "SKQUAD_DEFAULT_MODEL", Value: agent.Spec.DefaultModel},
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
			},
			LivenessProbe:  httpProbe("/healthz"),
			ReadinessProbe: httpProbe("/readyz"),
		}
		volumes := agentSecretVolumes(&agent)
		if len(volumes) > 0 {
			container.VolumeMounts = agentSecretVolumeMounts(&agent)
			deployment.Spec.Template.Spec.Volumes = volumes
		} else {
			deployment.Spec.Template.Spec.Volumes = nil
		}
		deployment.Spec.Template.Spec.Containers = []corev1.Container{container}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	agent.Status.ReadyDeployment = deployment.Name
	agent.Status.Replicas = replicas
	agent.Status.Ready = true
	agent.Status.Phase = "Ready"
	agent.Status.Reason = agentReadyReason(&agent, replicas)
	updateIdleSince(&agent, time.Now)
	agent.Status.UpdatedAt = metav1.Now()
	setCondition(&agent.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             agent.Status.Reason,
		Message:            fmt.Sprintf("Deployment %s/%s is ready", namespace, deployment.Name),
		ObservedGeneration: agent.Generation,
	})
	if err := r.Status().Update(ctx, &agent); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	return idleRequeue(&agent, replicas, time.Now), nil
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

// SetupWithManager registers the Agent controller with a controller-runtime
// manager.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&skquadv1.Agent{}).
		Complete(r)
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
		"skquad.io/agent-id":           agent.Spec.AgentID,
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

func agentReadyReason(agent *skquadv1.Agent, replicas int32) string {
	if agent.Spec.DesiredActive {
		return "DeploymentReady"
	}
	if replicas > 0 {
		return "IdleTimeoutWaiting"
	}
	return "ScaledToZero"
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
			Name: "agent-credential",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: agent.Spec.CredentialSecret,
			}},
		})
	}
	if agent.Spec.VirtualKeySecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "agent-virtual-key",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: agent.Spec.VirtualKeySecret,
			}},
		})
	}
	return volumes
}

func agentSecretVolumeMounts(agent *skquadv1.Agent) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	if agent.Spec.CredentialSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "agent-credential",
			MountPath: credentialsMount + "/agent",
			ReadOnly:  true,
		})
	}
	if agent.Spec.VirtualKeySecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "agent-virtual-key",
			MountPath: credentialsMount + "/llm-gateway",
			ReadOnly:  true,
		})
	}
	return mounts
}
