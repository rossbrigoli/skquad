// Package controller contains skquad Kubernetes reconcilers.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

const (
	managedBy                 = "skquad-operator"
	squadFinalizer            = "skquad.io/squad-cleanup"
	agentServiceAccountName   = "skquad-agent"
	apiSecretWriterRoleName   = "skquad-api-agent-secret-writer"
	apiSecretWriterBinding    = "skquad-api-agent-secret-writer"
	defaultDenyPolicyName     = "default-deny"
	dnsEgressPolicyName       = "allow-dns-egress"
	platformEgressPolicyName  = "allow-skquad-platform-egress"
	grantedEgressPolicyName   = "allow-granted-egress"
	defaultSquadQuotaName     = "skquad-squad-quota"
	defaultSquadPodQuota      = "20"
	defaultSquadCPURequests   = "4"
	defaultSquadMemoryRequest = "8Gi"
	defaultSquadCPULimits     = "8"
	defaultSquadMemoryLimits  = "16Gi"
)

// SquadReconciler reconciles Squad resources into isolated Kubernetes
// namespaces.
type SquadReconciler struct {
	client.Client
	Scheme                      *runtime.Scheme
	APIServerServiceAccountName string
}

// Reconcile ensures the squad namespace exists and records basic status.
func (r *SquadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var squad skquadv1.Squad
	if err := r.Get(ctx, req.NamespacedName, &squad); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if squad.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.AddFinalizer(&squad, squadFinalizer) {
			if err := r.Update(ctx, &squad); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		if controllerutil.ContainsFinalizer(&squad, squadFinalizer) {
			if err := r.cleanupSquad(ctx, &squad); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&squad, squadFinalizer)
			if err := r.Update(ctx, &squad); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	namespaceName := SquadNamespace(&squad)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, namespace, func() error {
		ensureSquadLabels(&namespace.Labels, &squad)
		return nil
	}); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureAgentServiceAccount(ctx, &squad, namespaceName); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureAPISecretWriterRBAC(ctx, &squad, namespaceName); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureDefaultDenyNetworkPolicy(ctx, &squad, namespaceName); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureDNSEgressNetworkPolicy(ctx, &squad, namespaceName); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensurePlatformEgressNetworkPolicy(ctx, &squad, namespaceName); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureGrantedEgressNetworkPolicy(ctx, &squad, namespaceName); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureResourceQuota(ctx, &squad, namespaceName); err != nil {
		return ctrl.Result{}, err
	}

	squad.Status.Namespace = namespaceName
	squad.Status.Ready = true
	squad.Status.Phase = "Ready"
	squad.Status.Reason = "BaseResourcesReady"
	squad.Status.UpdatedAt = metav1.Now()
	setCondition(&squad.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "BaseResourcesReady",
		Message:            fmt.Sprintf("Namespace %s base resources are ready", namespaceName),
		ObservedGeneration: squad.Generation,
	})
	if err := r.Status().Update(ctx, &squad); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *SquadReconciler) cleanupSquad(ctx context.Context, squad *skquadv1.Squad) error {
	namespaceName := SquadNamespace(squad)
	for _, obj := range []client.Object{
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: defaultDenyPolicyName, Namespace: namespaceName}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: dnsEgressPolicyName, Namespace: namespaceName}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: platformEgressPolicyName, Namespace: namespaceName}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: grantedEgressPolicyName, Namespace: namespaceName}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: apiSecretWriterBinding, Namespace: namespaceName}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: apiSecretWriterRoleName, Namespace: namespaceName}},
		&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: defaultSquadQuotaName, Namespace: namespaceName}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: agentServiceAccountName, Namespace: namespaceName}},
	} {
		if err := deleteIfExists(ctx, r.Client, obj); err != nil {
			return err
		}
	}
	return deleteIfExists(ctx, r.Client, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}})
}

func (r *SquadReconciler) ensureAgentServiceAccount(ctx context.Context, squad *skquadv1.Squad, namespace string) error {
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agentServiceAccountName, Namespace: namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, serviceAccount, func() error {
		ensureSquadLabels(&serviceAccount.Labels, squad)
		// Agent pods receive credentials via projected Secret volumes and never
		// call the Kubernetes API; the shared agent SA carries no RBAC, so do
		// not even place its token in the pod.
		serviceAccount.AutomountServiceAccountToken = boolPtr(false)
		return nil
	})
	return err
}

func (r *SquadReconciler) ensureAPISecretWriterRBAC(ctx context.Context, squad *skquadv1.Squad, namespace string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: apiSecretWriterRoleName, Namespace: namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		ensureSquadLabels(&role.Labels, squad)
		role.Rules = []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{
				"secrets",
			},
			Verbs: []string{
				"get",
				"create",
				"patch",
				"update",
				"delete",
			},
		}}
		return nil
	}); err != nil {
		return err
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: apiSecretWriterBinding, Namespace: namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		ensureSquadLabels(&binding.Labels, squad)
		binding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     apiSecretWriterRoleName,
		}
		binding.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      r.apiServerServiceAccountName(),
			Namespace: squad.Namespace,
		}}
		return nil
	})
	return err
}

func (r *SquadReconciler) ensureDefaultDenyNetworkPolicy(ctx context.Context, squad *skquadv1.Squad, namespace string) error {
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: defaultDenyPolicyName, Namespace: namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		ensureSquadLabels(&policy.Labels, squad)
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		}
		return nil
	})
	return err
}

func (r *SquadReconciler) ensureDNSEgressNetworkPolicy(ctx context.Context, squad *skquadv1.Squad, namespace string) error {
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: dnsEgressPolicyName, Namespace: namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		ensureSquadLabels(&policy.Labels, squad)
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": "kube-system",
						},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					networkPolicyPort(corev1.ProtocolUDP, 53),
					networkPolicyPort(corev1.ProtocolTCP, 53),
				},
			}},
		}
		return nil
	})
	return err
}

func (r *SquadReconciler) ensurePlatformEgressNetworkPolicy(ctx context.Context, squad *skquadv1.Squad, namespace string) error {
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: platformEgressPolicyName, Namespace: namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		ensureSquadLabels(&policy.Labels, squad)
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": squad.Namespace,
						},
					},
					PodSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{{
							Key:      "app.kubernetes.io/component",
							Operator: metav1.LabelSelectorOpIn,
							Values: []string{
								"api-server",
								"llm-gateway",
							},
						}},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					networkPolicyNamedPort(corev1.ProtocolTCP, "http"),
				},
			}},
		}
		return nil
	})
	return err
}

func (r *SquadReconciler) ensureResourceQuota(ctx context.Context, squad *skquadv1.Squad, namespace string) error {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: defaultSquadQuotaName, Namespace: namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		ensureSquadLabels(&quota.Labels, squad)
		quota.Spec.Hard = corev1.ResourceList{
			corev1.ResourcePods:           resource.MustParse(defaultSquadPodQuota),
			corev1.ResourceRequestsCPU:    resource.MustParse(defaultSquadCPURequests),
			corev1.ResourceRequestsMemory: resource.MustParse(defaultSquadMemoryRequest),
			corev1.ResourceLimitsCPU:      resource.MustParse(defaultSquadCPULimits),
			corev1.ResourceLimitsMemory:   resource.MustParse(defaultSquadMemoryLimits),
		}
		return nil
	})
	return err
}

// ensureGrantedEgressNetworkPolicy renders the per-squad egress allowlist
// declared in operatingModel.egress.allow[] into the allow-granted-egress
// NetworkPolicy. It is fail-closed: an invalid grant aborts reconciliation
// without widening any policy, and an empty allowlist removes the policy so
// only the platform defaults remain.
func (r *SquadReconciler) ensureGrantedEgressNetworkPolicy(ctx context.Context, squad *skquadv1.Squad, namespace string) error {
	grants, err := parseGrantedEgress(squad)
	if err != nil {
		return fmt.Errorf("squad %s: %w", squad.Name, err)
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: grantedEgressPolicyName, Namespace: namespace},
	}
	if len(grants) == 0 {
		return deleteIfExists(ctx, r.Client, policy)
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		ensureSquadLabels(&policy.Labels, squad)
		egress := make([]networkingv1.NetworkPolicyEgressRule, 0, len(grants))
		for _, grant := range grants {
			rule := networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{{
					IPBlock: &networkingv1.IPBlock{CIDR: grant.CIDR, Except: grant.Except},
				}},
			}
			for _, port := range grant.Ports {
				rule.Ports = append(rule.Ports, networkPolicyPort(corev1.ProtocolTCP, port))
			}
			egress = append(egress, rule)
		}
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		}
		return nil
	})
	return err
}

// parseGrantedEgress extracts and validates the egress allowlist from the
// squad operating model. Absent or empty is not an error; malformed JSON,
// bad CIDRs, or out-of-range ports are.
func parseGrantedEgress(squad *skquadv1.Squad) ([]skquadv1.EgressGrant, error) {
	if len(squad.Spec.OperatingModel.Raw) == 0 {
		return nil, nil
	}
	var model struct {
		Egress *skquadv1.SquadEgress `json:"egress,omitempty"`
	}
	if err := json.Unmarshal(squad.Spec.OperatingModel.Raw, &model); err != nil {
		return nil, fmt.Errorf("invalid operatingModel JSON: %w", err)
	}
	if model.Egress == nil || len(model.Egress.Allow) == 0 {
		return nil, nil
	}
	for i, grant := range model.Egress.Allow {
		if _, _, err := net.ParseCIDR(grant.CIDR); err != nil {
			return nil, fmt.Errorf("egress.allow[%d]: invalid cidr %q", i, grant.CIDR)
		}
		for _, except := range grant.Except {
			if _, _, err := net.ParseCIDR(except); err != nil {
				return nil, fmt.Errorf("egress.allow[%d]: invalid except cidr %q", i, except)
			}
		}
		for _, port := range grant.Ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("egress.allow[%d]: port %d out of range", i, port)
			}
		}
	}
	return model.Egress.Allow, nil
}

func networkPolicyPort(protocol corev1.Protocol, port int) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{
		Protocol: &protocol,
		Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: int32(port)},
	}
}

func networkPolicyNamedPort(protocol corev1.Protocol, port string) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{
		Protocol: &protocol,
		Port:     &intstr.IntOrString{Type: intstr.String, StrVal: port},
	}
}

func (r *SquadReconciler) apiServerServiceAccountName() string {
	if r.APIServerServiceAccountName != "" {
		return r.APIServerServiceAccountName
	}
	return "skquad-api-server"
}

func deleteIfExists(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func boolPtr(v bool) *bool { return &v }

// SetupWithManager registers the Squad controller with a controller-runtime
// manager.
func (r *SquadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&skquadv1.Squad{}).
		Complete(r)
}

// SquadNamespace returns the namespace reconciled for a Squad.
func SquadNamespace(squad *skquadv1.Squad) string {
	if squad.Spec.Namespace != "" {
		return squad.Spec.Namespace
	}
	if squad.Spec.SquadID != "" {
		return "squad-" + squad.Spec.SquadID
	}
	return "squad-" + squad.Name
}

func setCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	condition.LastTransitionTime = metav1.Now()
	for i := range *conditions {
		if (*conditions)[i].Type == condition.Type {
			(*conditions)[i] = condition
			return
		}
	}
	*conditions = append(*conditions, condition)
}

func ensureSquadLabels(labels *map[string]string, squad *skquadv1.Squad) {
	if *labels == nil {
		*labels = map[string]string{}
	}
	(*labels)["app.kubernetes.io/managed-by"] = managedBy
	(*labels)["skquad.io/squad-id"] = squad.Spec.SquadID
	(*labels)["skquad.io/squad-resource"] = squad.Name
}
