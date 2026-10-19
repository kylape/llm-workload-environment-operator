package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ReadyCondition            = "Ready"
	ArgoCDRegisteredCondition = "ArgoCDRegistered"
)

type EnvironmentMode string

const (
	EnvironmentModeNamespace EnvironmentMode = "Namespace"
	EnvironmentModeVCluster  EnvironmentMode = "VCluster"
)

type LLMWorkloadEnvironmentSpec struct {
	// Mode selects a direct host namespace or an isolated vCluster.
	// +kubebuilder:default=Namespace
	// +kubebuilder:validation:Enum=Namespace;VCluster
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="mode is immutable"
	Mode EnvironmentMode `json:"mode,omitempty"`

	// ClusterQueue is the Kueue ClusterQueue that backs the environment's
	// default LocalQueue.
	// +kubebuilder:default=inference-workspaces
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterQueue is immutable"
	ClusterQueue string `json:"clusterQueue,omitempty"`
}

type LLMWorkloadEnvironmentStatus struct {
	// NamespaceRef identifies the backing host-cluster namespace.
	NamespaceRef *corev1.LocalObjectReference `json:"namespaceRef,omitempty"`

	// Kubeconfig contains the base64-encoded kubeconfig for a ready environment.
	Kubeconfig []byte `json:"kubeconfig,omitempty"`

	// ArgoCDClusterSecretRef records the registration Secret to reconcile or clean
	// up, including when the integration is disabled or its namespace changes.
	// The reference is persisted before Secret creation; use ArgoCDRegistered
	// to determine whether registration has succeeded.
	ArgoCDClusterSecretRef *corev1.SecretReference `json:"argoCDClusterSecretRef,omitempty"`

	// Conditions contains environment readiness and Argo CD registration status.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=llmwe;lwe
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 59",message="environment names must be at most 59 characters to allow the env- prefix"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Namespace",type="string",JSONPath=".status.namespaceRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type LLMWorkloadEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LLMWorkloadEnvironmentSpec   `json:"spec,omitempty"`
	Status LLMWorkloadEnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type LLMWorkloadEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LLMWorkloadEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LLMWorkloadEnvironment{}, &LLMWorkloadEnvironmentList{})
}
