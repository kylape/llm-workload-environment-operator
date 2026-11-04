package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ReadyCondition            = "Ready"
	APIAccessibleCondition    = "APIAccessible"
	ArgoCDRegisteredCondition = "ArgoCDRegistered"
	ExpirationReadyCondition  = "ExpirationReady"
	ExpiredCondition          = "Expired"
)

type EnvironmentMode string

const (
	EnvironmentModeNamespace  EnvironmentMode = "Namespace"
	EnvironmentModeVCluster   EnvironmentMode = "VCluster"
	EnvironmentModeHyperShift EnvironmentMode = "HyperShift"
)

// +kubebuilder:validation:XValidation:rule="!has(self.pullSecret) || size(self.pullSecret.name) > 0",message="pullSecret.name must be set when pullSecret is specified"
type HyperShiftEnvironmentSpec struct {
	// Profile selects administrator-managed release, placement and worker sizing.
	// +kubebuilder:default=default
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Profile string `json:"profile,omitempty"`

	// Size selects the administrator-configured CPU and memory shape for each worker.
	// +kubebuilder:default=small
	// +kubebuilder:validation:Enum=small;medium;large
	Size HyperShiftSize `json:"size,omitempty"`

	// Replicas is the desired number of KubeVirt worker nodes, bounded by the profile.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas *int32 `json:"replicas,omitempty"`

	// PullSecret optionally selects a Docker config Secret in the LWE's namespace
	// for HostedCluster.spec.pullSecret. When omitted, the administrator profile's
	// pullSecret is used.
	// +optional
	PullSecret *corev1.LocalObjectReference `json:"pullSecret,omitempty"`

	// ImageDigestMirrorSet configures the digest-only image mirrors on HyperShift workers.
	// +optional
	ImageDigestMirrorSet *ImageDigestMirrorSetSpec `json:"imageDigestMirrorSet,omitempty"`
}

// HyperShiftSize selects a coarse-grained worker size configured by the administrator.
type HyperShiftSize string

const (
	HyperShiftSizeSmall  HyperShiftSize = "small"
	HyperShiftSizeMedium HyperShiftSize = "medium"
	HyperShiftSizeLarge  HyperShiftSize = "large"
)

// ImageDigestMirrorSetSpec contains the spec fields for an ImageDigestMirrorSet
// delivered to the HyperShift worker NodePool.
type ImageDigestMirrorSetSpec struct {
	// ImageDigestMirrors lists the source repositories and their digest mirrors.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=255
	// +listType=atomic
	ImageDigestMirrors []ImageDigestMirror `json:"imageDigestMirrors"`
}

// ImageDigestMirror defines one source repository and its ordered mirror list.
type ImageDigestMirror struct {
	// Source is the registry or repository used in image references.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Source string `json:"source"`

	// Mirrors are tried in order when pulling an image by digest.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=255
	// +listType=atomic
	Mirrors []string `json:"mirrors"`

	// MirrorSourcePolicy controls whether image pulls may fall back to Source.
	// It defaults to NeverContactSource so pulls fail if all mirrors fail.
	// +kubebuilder:default=NeverContactSource
	MirrorSourcePolicy MirrorSourcePolicy `json:"mirrorSourcePolicy,omitempty"`
}

// MirrorSourcePolicy controls whether a digest pull can fall back to its source.
// +kubebuilder:validation:Enum=NeverContactSource;AllowContactingSource
type MirrorSourcePolicy string

const (
	// NeverContactSource prevents fallback to the original source repository.
	NeverContactSource MirrorSourcePolicy = "NeverContactSource"
	// AllowContactingSource permits fallback to the original source repository.
	AllowContactingSource MirrorSourcePolicy = "AllowContactingSource"
)

// +kubebuilder:validation:XValidation:rule="!has(self.hypershift) || self.mode == 'HyperShift'",message="hypershift options require HyperShift mode"
// +kubebuilder:validation:XValidation:rule="self.mode != 'HyperShift' || (has(self.hypershift) && has(self.hypershift.profile) ? self.hypershift.profile : 'default') == (has(oldSelf.hypershift) && has(oldSelf.hypershift.profile) ? oldSelf.hypershift.profile : 'default')",message="HyperShift profile is immutable"
// +kubebuilder:validation:XValidation:rule="self.mode != 'HyperShift' || (has(self.hypershift) && has(self.hypershift.size) ? self.hypershift.size : 'small') == (has(oldSelf.hypershift) && has(oldSelf.hypershift.size) ? oldSelf.hypershift.size : 'small')",message="HyperShift size is immutable"
type LLMWorkloadEnvironmentSpec struct {
	// Mode selects a host namespace, vCluster, or hosted OpenShift cluster.
	// +kubebuilder:default=Namespace
	// +kubebuilder:validation:Enum=Namespace;VCluster;HyperShift
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="mode is immutable"
	Mode EnvironmentMode `json:"mode,omitempty"`

	// Duration is the requested lifetime of the environment, using Go time.Duration syntax.
	// If omitted, the operator uses its configured minimum duration.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Duration string `json:"duration,omitempty"`

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
	// ExpirationTimestamp is the absolute expiration time currently enforced by the operator.
	// Changing spec.duration updates this timestamp only when the new duration is valid.
	ExpirationTimestamp *metav1.Time `json:"expirationTimestamp,omitempty"`

	// ExpirationRemaining is the rounded-up time until expiration, refreshed by the expiration monitor.
	ExpirationRemaining string `json:"expirationRemaining,omitempty"`

	// LastObservedDuration records the last requested duration evaluated by the operator,
	// including invalid values that did not change ExpirationTimestamp.
	LastObservedDuration string `json:"lastObservedDuration,omitempty"`

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
// +kubebuilder:printcolumn:name="Expires",type="string",JSONPath=".status.expirationRemaining"
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
