package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ReadyCondition            = "Ready"
	APIAccessibleCondition    = "APIAccessible"
	ArgoCDRegisteredCondition = "ArgoCDRegistered"
)

type EnvironmentMode string

const (
	EnvironmentModeNamespace  EnvironmentMode = "Namespace"
	EnvironmentModeVCluster   EnvironmentMode = "VCluster"
	EnvironmentModeHyperShift EnvironmentMode = "HyperShift"
)

type HyperShiftEnvironmentSpec struct {
	// Profile selects administrator-managed release, placement and worker sizing.
	// +kubebuilder:default=default
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Profile string `json:"profile,omitempty"`

	// Replicas is the desired number of KubeVirt worker nodes, bounded by the profile.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas *int32 `json:"replicas,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.hypershift) || self.mode == 'HyperShift'",message="hypershift options require HyperShift mode"
// +kubebuilder:validation:XValidation:rule="self.mode != 'HyperShift' || (has(self.hypershift) && has(self.hypershift.profile) ? self.hypershift.profile : 'default') == (has(oldSelf.hypershift) && has(oldSelf.hypershift.profile) ? oldSelf.hypershift.profile : 'default')",message="HyperShift profile is immutable"
type LLMWorkloadEnvironmentSpec struct {
	// Mode selects a host namespace, vCluster, or hosted OpenShift cluster.
	// +kubebuilder:default=Namespace
	// +kubebuilder:validation:Enum=Namespace;VCluster;HyperShift
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="mode is immutable"
	Mode EnvironmentMode `json:"mode,omitempty"`

	// HyperShift configures a hosted OpenShift cluster with KubeVirt workers.
	// +optional
	HyperShift *HyperShiftEnvironmentSpec `json:"hypershift,omitempty"`

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

	// HostedClusterRef records the established hosted cluster identity.
	HostedClusterRef *corev1.ObjectReference `json:"hostedClusterRef,omitempty"`

	// NodePoolRef records the established worker pool identity.
	NodePoolRef *corev1.ObjectReference `json:"nodePoolRef,omitempty"`

	// Kubeconfig contains a base64-encoded kubeconfig for verified API access.
	// HyperShift may publish it before workers and full installation are Ready.
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
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.mode) || self.spec.mode != 'HyperShift' || size(self.metadata.name) <= 51",message="HyperShift environment names must be at most 51 characters for the hosted control-plane namespace"
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
