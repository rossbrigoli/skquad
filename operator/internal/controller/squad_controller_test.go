package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	apiServerName = "skquad-api-server"
)

func TestSquadReconcilerCreatesNamespace(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "squad-test",
			Namespace: testNamespace,
		},
		Spec: skquadv1.SquadSpec{
			SquadID:   "11111111-1111-1111-1111-111111111111",
			OwnerRef:  "owner-id",
			Namespace: "squad-runtime-test",
			Status:    "active",
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(squad).
		Build()
	reconciler := &SquadReconciler{
		Client:                      k8sClient,
		Scheme:                      scheme,
		APIServerServiceAccountName: apiServerName,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: squad.Name, Namespace: squad.Namespace},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Requeue {
		t.Fatalf("first reconcile result = %#v, want requeue after finalizer add", result)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: squad.Name, Namespace: squad.Namespace},
	}); err != nil {
		t.Fatal(err)
	}

	var namespace corev1.Namespace
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: squad.Spec.Namespace}, &namespace); err != nil {
		t.Fatal(err)
	}
	if got := namespace.Labels["skquad.io/squad-id"]; got != squad.Spec.SquadID {
		t.Fatalf("namespace squad label = %q, want %q", got, squad.Spec.SquadID)
	}

	var serviceAccount corev1.ServiceAccount
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agentServiceAccountName, Namespace: squad.Spec.Namespace}, &serviceAccount); err != nil {
		t.Fatal(err)
	}

	var secretRole rbacv1.Role
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: apiSecretWriterRoleName, Namespace: squad.Spec.Namespace}, &secretRole); err != nil {
		t.Fatal(err)
	}
	if got := secretRole.Rules[0].Resources; !containsString(got, "secrets") {
		t.Fatalf("secret writer resources = %#v, want secrets", got)
	}
	if got := secretRole.Rules[0].Verbs; containsString(got, "list") || !containsString(got, "patch") || !containsString(got, "delete") {
		t.Fatalf("secret writer verbs = %#v, want patch/delete without list", got)
	}
	var secretBinding rbacv1.RoleBinding
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: apiSecretWriterBinding, Namespace: squad.Spec.Namespace}, &secretBinding); err != nil {
		t.Fatal(err)
	}
	if got := secretBinding.Subjects[0]; got.Name != apiServerName || got.Namespace != squad.Namespace {
		t.Fatalf("secret writer subject = %#v, want skquad-system/skquad-api-server", got)
	}

	var policy networkingv1.NetworkPolicy
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: defaultDenyPolicyName, Namespace: squad.Spec.Namespace}, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) != 0 {
		t.Fatalf("default deny policy has ingress=%d egress=%d, want both empty", len(policy.Spec.Ingress), len(policy.Spec.Egress))
	}
	if got, want := policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}; !policyTypesEqual(got, want) {
		t.Fatalf("policy types = %#v, want %#v", got, want)
	}

	var dnsPolicy networkingv1.NetworkPolicy
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: dnsEgressPolicyName, Namespace: squad.Spec.Namespace}, &dnsPolicy); err != nil {
		t.Fatal(err)
	}
	if got := len(dnsPolicy.Spec.Egress); got != 1 {
		t.Fatalf("dns policy egress rules = %d, want 1", got)
	}
	if got := len(dnsPolicy.Spec.Egress[0].Ports); got != 2 {
		t.Fatalf("dns policy ports = %d, want 2", got)
	}
	if got := dnsPolicy.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "kube-system" {
		t.Fatalf("dns egress namespace = %q, want kube-system", got)
	}

	var platformPolicy networkingv1.NetworkPolicy
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: platformEgressPolicyName, Namespace: squad.Spec.Namespace}, &platformPolicy); err != nil {
		t.Fatal(err)
	}
	if got := platformPolicy.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != squad.Namespace {
		t.Fatalf("platform egress namespace = %q, want %q", got, squad.Namespace)
	}
	requirement := platformPolicy.Spec.Egress[0].To[0].PodSelector.MatchExpressions[0]
	if got := requirement.Key; got != "app.kubernetes.io/component" {
		t.Fatalf("platform egress selector key = %q, want app.kubernetes.io/component", got)
	}
	if !containsString(requirement.Values, "api-server") || !containsString(requirement.Values, "llm-gateway") {
		t.Fatalf("platform egress selector values = %#v, want api-server and llm-gateway", requirement.Values)
	}
	if got := platformPolicy.Spec.Egress[0].Ports[0].Port.StrVal; got != "http" {
		t.Fatalf("platform egress port = %q, want http", got)
	}

	var quota corev1.ResourceQuota
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: defaultSquadQuotaName, Namespace: squad.Spec.Namespace}, &quota); err != nil {
		t.Fatal(err)
	}
	if got := quota.Spec.Hard.Pods().String(); got != defaultSquadPodQuota {
		t.Fatalf("pod quota = %q, want %q", got, defaultSquadPodQuota)
	}

	var updatedSquad skquadv1.Squad
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: squad.Name, Namespace: squad.Namespace}, &updatedSquad); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&updatedSquad, squadFinalizer) {
		t.Fatalf("squad finalizers = %#v, want %q", updatedSquad.Finalizers, squadFinalizer)
	}
}

func TestSquadReconcilerFinalizerDeletesManagedResources(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "squad-delete",
			Namespace:  testNamespace,
			Finalizers: []string{squadFinalizer},
		},
		Spec: skquadv1.SquadSpec{
			SquadID:   "33333333-3333-3333-3333-333333333333",
			Namespace: "squad-delete-test",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			squad,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: squad.Spec.Namespace}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: agentServiceAccountName, Namespace: squad.Spec.Namespace}},
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: apiSecretWriterRoleName, Namespace: squad.Spec.Namespace}},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: apiSecretWriterBinding, Namespace: squad.Spec.Namespace}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: defaultDenyPolicyName, Namespace: squad.Spec.Namespace}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: dnsEgressPolicyName, Namespace: squad.Spec.Namespace}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: platformEgressPolicyName, Namespace: squad.Spec.Namespace}},
			&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: defaultSquadQuotaName, Namespace: squad.Spec.Namespace}},
		).
		Build()
	reconciler := &SquadReconciler{Client: k8sClient, Scheme: scheme}

	if err := k8sClient.Delete(context.Background(), squad); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: squad.Name, Namespace: squad.Namespace},
	}); err != nil {
		t.Fatal(err)
	}

	for _, obj := range []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: agentServiceAccountName, Namespace: squad.Spec.Namespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: apiSecretWriterRoleName, Namespace: squad.Spec.Namespace}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: apiSecretWriterBinding, Namespace: squad.Spec.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: defaultDenyPolicyName, Namespace: squad.Spec.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: dnsEgressPolicyName, Namespace: squad.Spec.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: platformEgressPolicyName, Namespace: squad.Spec.Namespace}},
		&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: defaultSquadQuotaName, Namespace: squad.Spec.Namespace}},
	} {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Fatalf("object %s/%s still exists or lookup failed: %v", obj.GetNamespace(), obj.GetName(), err)
		}
	}
	var namespace corev1.Namespace
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: squad.Spec.Namespace}, &namespace); !apierrors.IsNotFound(err) {
		t.Fatalf("namespace still exists or lookup failed: %v", err)
	}
}

func TestSquadNamespaceFallsBackToSquadID(t *testing.T) {
	t.Parallel()

	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{Name: "fallback"},
		Spec: skquadv1.SquadSpec{
			SquadID: "22222222-2222-2222-2222-222222222222",
		},
	}
	if got, want := SquadNamespace(squad), "squad-22222222-2222-2222-2222-222222222222"; got != want {
		t.Fatalf("SquadNamespace() = %q, want %q", got, want)
	}
}

func policyTypesEqual(a, b []networkingv1.PolicyType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func squadWithEgress(t *testing.T, operatingModel string) *skquadv1.Squad {
	t.Helper()
	squad := &skquadv1.Squad{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "squad-egress-test",
			Namespace: testNamespace,
		},
		Spec: skquadv1.SquadSpec{
			SquadID:   "33333333-3333-3333-3333-333333333333",
			OwnerRef:  "owner-id",
			Namespace: "squad-egress-test-ns",
			Status:    "active",
		},
	}
	if operatingModel != "" {
		squad.Spec.OperatingModel = apiextensionsv1.JSON{Raw: []byte(operatingModel)}
	}
	return squad
}

func reconcileSquadTwice(t *testing.T, reconciler *SquadReconciler, squad *skquadv1.Squad) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: squad.Name, Namespace: squad.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
}

func TestSquadReconcilerGrantedEgressPolicy(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	squad := squadWithEgress(t, `{"mission":"x","egress":{"allow":[`+
		`{"cidr":"93.184.215.208/29","except":["93.184.215.216/31"],"ports":[443],"description":"git host"},`+
		`{"cidr":"140.82.112.0/20"}]}}`)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(squad).Build()
	reconciler := &SquadReconciler{Client: k8sClient, Scheme: scheme, APIServerServiceAccountName: apiServerName}
	reconcileSquadTwice(t, reconciler, squad)

	var policy networkingv1.NetworkPolicy
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: grantedEgressPolicyName, Namespace: squad.Spec.Namespace}, &policy); err != nil {
		t.Fatalf("granted egress policy missing: %v", err)
	}
	if len(policy.Spec.Egress) != 2 {
		t.Fatalf("granted egress rules = %d, want 2", len(policy.Spec.Egress))
	}
	first := policy.Spec.Egress[0]
	firstPeer := first.To[0].IPBlock
	if firstPeer == nil || firstPeer.CIDR != "93.184.215.208/29" {
		t.Fatalf("first cidr = %#v, want 93.184.215.208/29", firstPeer)
	}
	if len(firstPeer.Except) != 1 || firstPeer.Except[0] != "93.184.215.216/31" {
		t.Fatalf("first except = %#v", firstPeer.Except)
	}
	if len(first.Ports) != 1 || first.Ports[0].Port.IntVal != 443 || *first.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("first ports = %#v, want TCP/443", first.Ports)
	}
	second := policy.Spec.Egress[1]
	secondPeer := second.To[0].IPBlock
	if secondPeer == nil || secondPeer.CIDR != "140.82.112.0/20" || len(second.Ports) != 0 {
		t.Fatalf("second grant = %#v / peer %#v, want cidr 140.82.112.0/20 all ports", second, secondPeer)
	}

	var sa corev1.ServiceAccount
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: agentServiceAccountName, Namespace: squad.Spec.Namespace}, &sa); err != nil {
		t.Fatal(err)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Fatalf("agent SA automount = %v, want false", sa.AutomountServiceAccountToken)
	}
}

func TestSquadReconcilerRemovesStaleGrantedEgress(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	squad := squadWithEgress(t, `{"mission":"x"}`)
	stale := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: grantedEgressPolicyName, Namespace: squad.Spec.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
			}},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(squad, stale).Build()
	reconciler := &SquadReconciler{Client: k8sClient, Scheme: scheme, APIServerServiceAccountName: apiServerName}
	reconcileSquadTwice(t, reconciler, squad)

	var policy networkingv1.NetworkPolicy
	err := k8sClient.Get(context.Background(), client.ObjectKey{Name: grantedEgressPolicyName, Namespace: squad.Spec.Namespace}, &policy)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("stale granted egress policy still present (err=%v)", err)
	}
}

func TestSquadReconcilerInvalidGrantedEgressFailsClosed(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := skquadv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"bad cidr":       `{"egress":{"allow":[{"cidr":"not-a-cidr"}]}}`,
		"bad except":     `{"egress":{"allow":[{"cidr":"10.0.0.0/8","except":["nope"]}]}}`,
		"bad port":       `{"egress":{"allow":[{"cidr":"10.0.0.0/8","ports":[70000]}]}}`,
		"malformed json": `{"egress":{`,
	}
	for name, model := range cases {
		squad := squadWithEgress(t, model)
		squad.Name = "squad-egress-bad-" + name
		squad.Spec.Namespace = "squad-egress-bad-ns"
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(squad).Build()
		reconciler := &SquadReconciler{Client: k8sClient, Scheme: scheme, APIServerServiceAccountName: apiServerName}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: squad.Name, Namespace: squad.Namespace}}
		if _, err := reconciler.Reconcile(context.Background(), req); err == nil {
			// first reconcile only adds the finalizer; second must fail
			if _, err := reconciler.Reconcile(context.Background(), req); err == nil {
				t.Fatalf("%s: expected reconcile error, got nil", name)
			}
		}
		var policy networkingv1.NetworkPolicy
		if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: grantedEgressPolicyName, Namespace: squad.Spec.Namespace}, &policy); err == nil {
			t.Fatalf("%s: granted egress policy was created despite invalid grant", name)
		}
	}
}
