// Package v1 defines the skquad.io/v1 custom resources (Squad, Agent).
// See docs/deployment-operator.md §2.
package v1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// Group is the Kubernetes API group for skquad custom resources.
	Group = "skquad.io"
	// Version is the Kubernetes API version for skquad custom resources.
	Version = "v1"
)

var (
	// GroupVersion identifies the skquad Kubernetes API.
	GroupVersion = schema.GroupVersion{Group: Group, Version: Version}
	// SchemeBuilder registers skquad API types with a Kubernetes scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme registers skquad API types with a Kubernetes scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &Squad{}, &SquadList{}, &Agent{}, &AgentList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

// SquadSpec declares the desired state for a squad namespace and base
// resources.
type SquadSpec struct {
	SquadID        string               `json:"squadId"`
	OwnerRef       string               `json:"ownerRef"`
	Namespace      string               `json:"namespace,omitempty"`
	Mission        string               `json:"mission,omitempty"`
	OperatingModel apiextensionsv1.JSON `json:"operatingModel,omitempty"`
	Status         string               `json:"status,omitempty"`
}

// SquadStatus reports reconciliation state for a Squad.
type SquadStatus struct {
	Namespace  string             `json:"namespace,omitempty"`
	Ready      bool               `json:"ready,omitempty"`
	Phase      string             `json:"phase,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	UpdatedAt  metav1.Time        `json:"updatedAt,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Squad is the custom resource the operator reconciles into a squad namespace.
type Squad struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SquadSpec   `json:"spec,omitempty"`
	Status SquadStatus `json:"status,omitempty"`
}

// SquadList is a list of Squad resources.
type SquadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Squad `json:"items"`
}

// AgentSpec declares the desired state for an agent Deployment.
type AgentSpec struct {
	AgentID           string               `json:"agentId"`
	SquadID           string               `json:"squadId"`
	Role              string               `json:"role,omitempty"`
	SystemPrompt      string               `json:"systemPrompt,omitempty"`
	DefaultProviderID string               `json:"defaultProviderId,omitempty"`
	DefaultModel      string               `json:"defaultModel,omitempty"`
	Image             string               `json:"image"`
	CredentialSecret  string               `json:"credentialSecret,omitempty"`
	VirtualKeySecret  string               `json:"virtualKeySecret,omitempty"`
	ControlPlaneURL   string               `json:"controlPlaneUrl,omitempty"`
	LLMGatewayURL     string               `json:"llmGatewayUrl,omitempty"`
	Permissions       apiextensionsv1.JSON `json:"permissions,omitempty"`
	IdleTimeout       string               `json:"idleTimeout,omitempty"`
	DesiredActive     bool                 `json:"desiredActive"`
}

// AgentStatus reports reconciliation state for an Agent.
type AgentStatus struct {
	ReadyDeployment string             `json:"readyDeployment,omitempty"`
	Replicas        int32              `json:"replicas,omitempty"`
	Ready           bool               `json:"ready,omitempty"`
	Phase           string             `json:"phase,omitempty"`
	Reason          string             `json:"reason,omitempty"`
	IdleSince       metav1.Time        `json:"idleSince,omitempty"`
	UpdatedAt       metav1.Time        `json:"updatedAt,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
}

// Agent is the custom resource the operator reconciles into an agent
// Deployment inside its squad namespace.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

// AgentList is a list of Agent resources.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Agent `json:"items"`
}

// DeepCopyObject implements runtime.Object.
func (in *Squad) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(Squad)
	*out = *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.OperatingModel = cloneJSON(in.Spec.OperatingModel)
	out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *SquadList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SquadList)
	*out = *in
	out.ListMeta = in.ListMeta
	out.Items = append([]Squad(nil), in.Items...)
	for i := range out.Items {
		copied := out.Items[i].DeepCopyObject().(*Squad)
		out.Items[i] = *copied
	}
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *Agent) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(Agent)
	*out = *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.Permissions = cloneJSON(in.Spec.Permissions)
	out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *AgentList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(AgentList)
	*out = *in
	out.ListMeta = in.ListMeta
	out.Items = append([]Agent(nil), in.Items...)
	for i := range out.Items {
		copied := out.Items[i].DeepCopyObject().(*Agent)
		out.Items[i] = *copied
	}
	return out
}

func cloneJSON(in apiextensionsv1.JSON) apiextensionsv1.JSON {
	if len(in.Raw) == 0 {
		return apiextensionsv1.JSON{}
	}
	return apiextensionsv1.JSON{Raw: append([]byte(nil), in.Raw...)}
}
