package controller

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	controllerclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// gatewayAccessFixture reconciles two Namespace-mode environments against a
// cluster that has the configured gateway namespace, and returns the client
// for inspection.
func gatewayAccessFixture(t *testing.T, reconcilerGatewayNamespaces []string) (controllerclient.Client, *LLMWorkloadEnvironmentReconciler, []*environmentv1alpha1.LLMWorkloadEnvironment) {
	t.Helper()
	ctx := context.Background()
	scheme := testScheme(t)

	first := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "requests", UID: types.UID("first-uid")},
		Spec:       environmentv1alpha1.LLMWorkloadEnvironmentSpec{ClusterQueue: "development"},
	}
	second := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "requests", UID: types.UID("second-uid")},
		Spec:       environmentv1alpha1.LLMWorkloadEnvironmentSpec{ClusterQueue: "development"},
	}
	clusterQueue := &kueuev1beta2.ClusterQueue{ObjectMeta: metav1.ObjectMeta{Name: "development"}}
	landingNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "requests",
		Annotations: map[string]string{
			AllowedClusterQueuesAnnotation: "development",
		},
	}}
	gatewayNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openshift-ingress"}}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&environmentv1alpha1.LLMWorkloadEnvironment{}).
		WithObjects(first, second, landingNamespace, clusterQueue, gatewayNamespace).
		Build()
	reconciler := testReconciler(t, client, scheme)
	reconciler.GatewayNamespaces = reconcilerGatewayNamespaces

	for _, environment := range []*environmentv1alpha1.LLMWorkloadEnvironment{first, second} {
		request := ctrl.Request{NamespacedName: types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}}
		for range 2 {
			if _, err := reconciler.Reconcile(ctx, request); err != nil {
				t.Fatalf("reconciling %s: %v", environment.Name, err)
			}
		}
	}
	return client, reconciler, []*environmentv1alpha1.LLMWorkloadEnvironment{first, second}
}

func TestConcurrentEnvironmentsEachGetGatewayAccessBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, _, environments := gatewayAccessFixture(t, []string{"openshift-ingress"})

	for _, environment := range environments {
		binding := &rbacv1.RoleBinding{}
		name := types.NamespacedName{Name: gatewayAccessBindingName(environment), Namespace: "openshift-ingress"}
		if err := client.Get(ctx, name, binding); err != nil {
			t.Fatalf("gateway access RoleBinding for %s: %v", environment.Name, err)
		}
		if binding.RoleRef.Name != GatewayAccessRoleName ||
			len(binding.Subjects) != 1 ||
			binding.Subjects[0].Name != NamespaceKubeconfigServiceAccountName ||
			binding.Subjects[0].Namespace != NamespacePrefix+environment.Name {
			t.Fatalf("unexpected gateway access binding for %s: %#v", environment.Name, binding)
		}
		if !environmentOwns(binding, environment) {
			t.Fatalf("gateway access binding for %s is not owned by it", environment.Name)
		}
	}

	// Both environments must report progress past gateway access (QueueNotReady
	// is the next gate in this fixture, which has no active LocalQueue).
	for _, environment := range environments {
		current := &environmentv1alpha1.LLMWorkloadEnvironment{}
		if err := client.Get(ctx, types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}, current); err != nil {
			t.Fatalf("get %s: %v", environment.Name, err)
		}
		if ready := findCondition(current, "Ready"); ready == nil || ready.Reason == "ReconciliationFailed" {
			t.Fatalf("environment %s stuck on gateway access: %#v", environment.Name, ready)
		}
	}
}

func findCondition(environment *environmentv1alpha1.LLMWorkloadEnvironment, conditionType string) *metav1.Condition {
	for i, condition := range environment.Status.Conditions {
		if condition.Type == conditionType {
			return &environment.Status.Conditions[i]
		}
	}
	return nil
}
