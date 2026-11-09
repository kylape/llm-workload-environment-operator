package controller

import (
	"context"
	"testing"

	envapi "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func peerGroupClusterNamespace(t *testing.T, r *LLMWorkloadEnvironmentReconciler) *corev1.Namespace {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "env-test-cluster"}}
	if err := r.Create(context.Background(), namespace); err != nil {
		t.Fatal(err)
	}
	return namespace
}

func getPeerGroupPolicy(t *testing.T, r *LLMWorkloadEnvironmentReconciler) *netv1.NetworkPolicy {
	t.Helper()
	policy := &netv1.NetworkPolicy{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: PeerGroupNetworkPolicyName, Namespace: "env-test-cluster"}, policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

func assertPeerGroupPolicySpec(t *testing.T, policy *netv1.NetworkPolicy, value string) {
	t.Helper()
	if !environmentOwnsAnnotations(policy) {
		t.Fatal("policy is not marked as environment-owned")
	}
	if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != netv1.PolicyTypeEgress {
		t.Fatalf("policy types: %v", policy.Spec.PolicyTypes)
	}
	if policy.Spec.PodSelector.MatchLabels[virtLauncherLabel] != "virt-launcher" || len(policy.Spec.PodSelector.MatchLabels) != 1 {
		t.Fatalf("pod selector: %v", policy.Spec.PodSelector.MatchLabels)
	}
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].To) != 1 {
		t.Fatalf("egress rules: %+v", policy.Spec.Egress)
	}
	peer := policy.Spec.Egress[0].To[0]
	if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels[PeerGroupLabel] != value || len(peer.NamespaceSelector.MatchLabels) != 1 {
		t.Fatalf("namespace selector: %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels[virtLauncherLabel] != "virt-launcher" || len(peer.PodSelector.MatchLabels) != 1 {
		t.Fatalf("peer pod selector: %+v", peer.PodSelector)
	}
	ports := policy.Spec.Egress[0].Ports
	if len(ports) != 3 {
		t.Fatalf("ports: %+v", ports)
	}
	want := []int32{500, 4500, 4490}
	for i, port := range ports {
		if port.Protocol == nil || *port.Protocol != corev1.ProtocolUDP {
			t.Fatalf("port %d protocol: %+v", i, port.Protocol)
		}
		if port.Port == nil || port.Port.Type != intstr.Int || port.Port.IntValue() != int(want[i]) {
			t.Fatalf("port %d: %+v", i, port.Port)
		}
	}
}

// environmentOwnsAnnotations reports whether the object carries the LWE
// ownership annotations for the test environment fixture.
func environmentOwnsAnnotations(object metav1.Object) bool {
	annotations := object.GetAnnotations()
	return annotations[EnvironmentOwnerAnnotation] == "requests/test" && annotations[EnvironmentUIDAnnotation] == "environment-uid"
}

func TestPeerGroupLabelAppliesNamespaceLabelAndPolicy(t *testing.T) {
	ctx := context.Background()
	r, e := hyperShiftFixture(t)
	peerGroupClusterNamespace(t, r)
	e.Labels = map[string]string{PeerGroupLabel: "grid-a"}
	if err := r.ensureHyperShiftPeerGroup(ctx, e, "env-test"); err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: "env-test-cluster"}, namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels[PeerGroupLabel] != "grid-a" {
		t.Fatalf("cluster namespace label: %v", namespace.Labels)
	}
	assertPeerGroupPolicySpec(t, getPeerGroupPolicy(t, r), "grid-a")

	// A changed value updates both the namespace label and the policy.
	e.Labels[PeerGroupLabel] = "grid-b"
	if err := r.ensureHyperShiftPeerGroup(ctx, e, "env-test"); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, types.NamespacedName{Name: "env-test-cluster"}, namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels[PeerGroupLabel] != "grid-b" {
		t.Fatalf("updated cluster namespace label: %v", namespace.Labels)
	}
	assertPeerGroupPolicySpec(t, getPeerGroupPolicy(t, r), "grid-b")
}

func TestPeerGroupLabelRemovalCleansUp(t *testing.T) {
	ctx := context.Background()
	r, e := hyperShiftFixture(t)
	peerGroupClusterNamespace(t, r)
	e.Labels = map[string]string{PeerGroupLabel: "grid-a"}
	if err := r.ensureHyperShiftPeerGroup(ctx, e, "env-test"); err != nil {
		t.Fatal(err)
	}
	getPeerGroupPolicy(t, r)

	delete(e.Labels, PeerGroupLabel)
	if err := r.ensureHyperShiftPeerGroup(ctx, e, "env-test"); err != nil {
		t.Fatal(err)
	}
	policy := &netv1.NetworkPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: PeerGroupNetworkPolicyName, Namespace: "env-test-cluster"}, policy); !apierrors.IsNotFound(err) {
		t.Fatalf("policy not removed: %v", err)
	}
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: "env-test-cluster"}, namespace); err != nil {
		t.Fatal(err)
	}
	if _, still := namespace.Labels[PeerGroupLabel]; still {
		t.Fatalf("namespace label not removed: %v", namespace.Labels)
	}
}

func TestPeerGroupUnlabelledEnvironmentTakesNoAction(t *testing.T) {
	ctx := context.Background()
	r, e := hyperShiftFixture(t)
	peerGroupClusterNamespace(t, r)
	if err := r.ensureHyperShiftPeerGroup(ctx, e, "env-test"); err != nil {
		t.Fatal(err)
	}
	policy := &netv1.NetworkPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: PeerGroupNetworkPolicyName, Namespace: "env-test-cluster"}, policy); !apierrors.IsNotFound(err) {
		t.Fatalf("policy created without a peer-group label: %v", err)
	}
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: "env-test-cluster"}, namespace); err != nil {
		t.Fatal(err)
	}
	if _, labeled := namespace.Labels[PeerGroupLabel]; labeled {
		t.Fatalf("namespace labeled without a peer-group label: %v", namespace.Labels)
	}
}

func TestPeerGroupWaitsForControlPlaneNamespace(t *testing.T) {
	ctx := context.Background()
	r, e := hyperShiftFixture(t)
	e.Labels = map[string]string{PeerGroupLabel: "grid-a"}
	// HyperShift has not created env-test-cluster yet.
	if err := r.ensureHyperShiftPeerGroup(ctx, e, "env-test"); err != nil {
		t.Fatal(err)
	}
	policy := &netv1.NetworkPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: PeerGroupNetworkPolicyName, Namespace: "env-test-cluster"}, policy); !apierrors.IsNotFound(err) {
		t.Fatalf("policy created before the control-plane namespace: %v", err)
	}
}

func TestPeerGroupRejectsForeignPolicy(t *testing.T) {
	ctx := context.Background()
	r, e := hyperShiftFixture(t)
	peerGroupClusterNamespace(t, r)
	foreign := &netv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: PeerGroupNetworkPolicyName, Namespace: "env-test-cluster"}}
	if err := r.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	e.Labels = map[string]string{PeerGroupLabel: "grid-a"}
	if err := r.ensureHyperShiftPeerGroup(ctx, e, "env-test"); err == nil {
		t.Fatal("foreign policy adopted")
	}
}

func TestPeerGroupManagedThroughHyperShiftReconcile(t *testing.T) {
	ctx := context.Background()
	r, e := hyperShiftFixture(t)
	peerGroupClusterNamespace(t, r)
	e.Labels = map[string]string{PeerGroupLabel: "grid-a"}
	if err := r.Update(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ensureHyperShift(ctx, e, "env-test"); err != nil {
		t.Fatal(err)
	}
	assertPeerGroupPolicySpec(t, getPeerGroupPolicy(t, r), "grid-a")
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: "env-test-cluster"}, namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels[PeerGroupLabel] != "grid-a" {
		t.Fatalf("cluster namespace label: %v", namespace.Labels)
	}

	// Finalization removes the artifacts even while the label is still set.
	if done, err := r.finalizeHyperShift(ctx, e, "env-test"); err != nil || done {
		t.Fatalf("finalize must first destroy the cluster: %v %v", done, err)
	}
	policy := &netv1.NetworkPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: PeerGroupNetworkPolicyName, Namespace: "env-test-cluster"}, policy); !apierrors.IsNotFound(err) {
		t.Fatalf("policy not removed during finalization: %v", err)
	}
	if err := r.Get(ctx, types.NamespacedName{Name: "env-test-cluster"}, namespace); err != nil {
		t.Fatal(err)
	}
	if _, labeled := namespace.Labels[PeerGroupLabel]; labeled {
		t.Fatalf("namespace label not removed during finalization: %v", namespace.Labels)
	}
}

func TestPeerGroupNotCreatedForNamespaceMode(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &envapi.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "example", Namespace: "requests", UID: types.UID("environment-uid"),
			Labels: map[string]string{PeerGroupLabel: "grid-a"},
		},
		Spec: envapi.LLMWorkloadEnvironmentSpec{ClusterQueue: "development"},
	}
	landingNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "requests",
		Annotations: map[string]string{AllowedClusterQueuesAnnotation: "development"},
	}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&envapi.LLMWorkloadEnvironment{}).
		WithObjects(environment, landingNamespace).
		Build()
	reconciler := testReconciler(t, c, scheme)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}}
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	policies := &netv1.NetworkPolicyList{}
	if err := c.List(ctx, policies); err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) != 0 {
		t.Fatalf("namespace mode created peer-group policies: %+v", policies.Items)
	}
}
