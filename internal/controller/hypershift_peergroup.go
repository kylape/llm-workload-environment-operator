package controller

import (
	"context"
	"fmt"
	"reflect"

	envapi "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// PeerGroupLabel marks LWEs whose HyperShift guests must reach each
	// other over the host pod network. All LWEs sharing a value form one
	// peer group.
	PeerGroupLabel = "llm.redhat.com/peer-group"
	// PeerGroupNetworkPolicyName is the additive NetworkPolicy the operator
	// maintains in each peer-group member's control-plane namespace. It
	// unions with HyperShift's managed "virt-launcher" policy and never
	// modifies it.
	PeerGroupNetworkPolicyName = "allow-inter-peer-group"
	// virtLauncherLabel selects KubeVirt virt-launcher pods, the pods that
	// back HyperShift guest nodes on the host pod network.
	virtLauncherLabel = "kubevirt.io"
)

// hyperShiftClusterNamespace returns the control-plane namespace HyperShift
// creates for a HostedCluster living in the given environment namespace.
func hyperShiftClusterNamespace(namespace string) string {
	return namespace + "-cluster"
}

// peerGroupNetworkPolicySpec returns the desired spec of the additive
// inter-peer-group policy. Only encapsulated-UDP tunnel ports are opened:
// NetworkPolicy cannot express native ESP (IP protocol 50), so consumers
// must force UDP encapsulation.
func peerGroupNetworkPolicySpec(value string) netv1.NetworkPolicySpec {
	udp := corev1.ProtocolUDP
	port := func(number int32) *intstr.IntOrString {
		p := intstr.FromInt32(number)
		return &p
	}
	return netv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{virtLauncherLabel: "virt-launcher"}},
		PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeEgress},
		Egress: []netv1.NetworkPolicyEgressRule{{
			To: []netv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{PeerGroupLabel: value}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{virtLauncherLabel: "virt-launcher"}},
			}},
			Ports: []netv1.NetworkPolicyPort{
				{Protocol: &udp, Port: port(500)},  // IKE
				{Protocol: &udp, Port: port(4500)}, // NAT-T / submariner cable
				{Protocol: &udp, Port: port(4490)}, // NAT discovery
			},
		}},
	}
}

// ensureHyperShiftPeerGroup reconciles the peer-group namespace label and the
// additive allow-inter-peer-group NetworkPolicy for a HyperShift-mode LWE. It
// is a no-op while HyperShift has not created the control-plane namespace
// yet, and it removes both artifacts when the LWE carries no peer-group label.
func (r *LLMWorkloadEnvironmentReconciler) ensureHyperShiftPeerGroup(
	ctx context.Context,
	environment *envapi.LLMWorkloadEnvironment,
	namespace string,
) error {
	value := environment.Labels[PeerGroupLabel]
	if value == "" {
		return r.removeHyperShiftPeerGroup(ctx, environment, namespace)
	}
	clusterNamespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: hyperShiftClusterNamespace(namespace)}, clusterNamespace); err != nil {
		if apierrors.IsNotFound(err) {
			// HyperShift creates env-<name>-cluster with the control plane.
			return nil
		}
		return err
	}
	if clusterNamespace.Labels[PeerGroupLabel] != value {
		before := clusterNamespace.DeepCopy()
		if clusterNamespace.Labels == nil {
			clusterNamespace.Labels = make(map[string]string)
		}
		clusterNamespace.Labels[PeerGroupLabel] = value
		if err := r.Patch(ctx, clusterNamespace, client.MergeFrom(before)); err != nil {
			return err
		}
	}
	policy := &netv1.NetworkPolicy{}
	key := types.NamespacedName{Name: PeerGroupNetworkPolicyName, Namespace: clusterNamespace.Name}
	err := r.hyperShiftReader().Get(ctx, key, policy)
	if apierrors.IsNotFound(err) {
		desired := peerGroupNetworkPolicySpec(value)
		created := &netv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: PeerGroupNetworkPolicyName, Namespace: clusterNamespace.Name, Annotations: environmentOwnershipAnnotations(environment)},
			Spec:       desired,
		}
		return r.Create(ctx, created)
	}
	if err != nil {
		return err
	}
	if !environmentOwns(policy, environment) {
		return fmt.Errorf("NetworkPolicy %s/%s is not controlled by this environment", clusterNamespace.Name, PeerGroupNetworkPolicyName)
	}
	desired := peerGroupNetworkPolicySpec(value)
	if !reflect.DeepEqual(policy.Spec, desired) {
		before := policy.DeepCopy()
		policy.Spec = desired
		markEnvironmentOwned(policy, environment)
		return r.Patch(ctx, policy, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	return nil
}

// removeHyperShiftPeerGroup deletes the operator-owned inter-peer-group
// NetworkPolicy and removes the peer-group label from the control-plane
// namespace. Missing objects are not errors; teardown and label removal
// share this path. A Forbidden read is also tolerated: when the operator's
// RBAC has not been extended with networkpolicies access, the operator
// cannot have created the policy, so unlabelled and deleting environments
// keep reconciling instead of failing.
func (r *LLMWorkloadEnvironmentReconciler) removeHyperShiftPeerGroup(
	ctx context.Context,
	environment *envapi.LLMWorkloadEnvironment,
	namespace string,
) error {
	name := hyperShiftClusterNamespace(namespace)
	policy := &netv1.NetworkPolicy{}
	if err := r.hyperShiftReader().Get(ctx, types.NamespacedName{Name: PeerGroupNetworkPolicyName, Namespace: name}, policy); err != nil {
		if !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
			return err
		}
	} else if environmentOwns(policy, environment) {
		if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	clusterNamespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, clusterNamespace); err != nil {
		return client.IgnoreNotFound(err)
	}
	if _, labeled := clusterNamespace.Labels[PeerGroupLabel]; labeled {
		before := clusterNamespace.DeepCopy()
		delete(clusterNamespace.Labels, PeerGroupLabel)
		return r.Patch(ctx, clusterNamespace, client.MergeFrom(before))
	}
	return nil
}
