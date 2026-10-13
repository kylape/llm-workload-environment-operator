package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/yaml"
)

func TestReconcileNamespaceEnvironment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "requests", UID: types.UID("environment-uid")},
		Spec: environmentv1alpha1.LLMWorkloadEnvironmentSpec{
			ClusterQueue: "development",
		},
	}
	clusterQueue := &kueuev1beta2.ClusterQueue{ObjectMeta: metav1.ObjectMeta{Name: "development"}}
	landingNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "requests",
		Annotations: map[string]string{
			AllowedClusterQueuesAnnotation: "development",
		},
	}}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&environmentv1alpha1.LLMWorkloadEnvironment{}).
		WithObjects(environment, landingNamespace, clusterQueue).
		Build()
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("adding finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("creating environment resources: %v", err)
	}

	namespace := &corev1.Namespace{}
	if err := client.Get(ctx, types.NamespacedName{Name: NamespacePrefix + "example"}, namespace); err != nil {
		t.Fatalf("environment namespace: %v", err)
	}
	if namespace.Labels[KueueManagedLabel] != "true" {
		t.Fatalf("environment namespace is not labeled for Kueue management: %#v", namespace.Labels)
	}
	if !environmentOwns(namespace, environment) || len(namespace.OwnerReferences) != 0 {
		t.Fatalf("environment namespace has unexpected ownership metadata: annotations=%#v owners=%#v", namespace.Annotations, namespace.OwnerReferences)
	}
	binding := &rbacv1.RoleBinding{}
	if err := client.Get(ctx, types.NamespacedName{Name: AccessBindingName, Namespace: namespace.Name}, binding); err != nil {
		t.Fatalf("environment access RoleBinding: %v", err)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != EnvironmentRoleName ||
		len(binding.Subjects) != 1 ||
		binding.Subjects[0].Name != NamespaceKubeconfigServiceAccountName || binding.Subjects[0].Namespace != namespace.Name {
		t.Fatalf("unexpected environment access binding: %#v", binding)
	}
	queue := &kueuev1beta2.LocalQueue{}
	if err := client.Get(ctx, types.NamespacedName{Name: LocalQueueName, Namespace: namespace.Name}, queue); err != nil {
		t.Fatalf("default LocalQueue: %v", err)
	}
	if queue.Spec.ClusterQueue != "development" {
		t.Fatalf("LocalQueue references %q, want %q", queue.Spec.ClusterQueue, "development")
	}

	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := client.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatalf("environment status: %v", err)
	}
	ready := apimeta.FindStatusCondition(current.Status.Conditions, environmentv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "QueueNotReady" {
		t.Fatalf("unexpected Ready condition: %#v", ready)
	}
}

func TestReconcileRejectsClusterQueueOutsideLandingNamespaceAllowlist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "example",
			Namespace:  "requests",
			UID:        types.UID("environment-uid"),
			Finalizers: []string{FinalizerName},
		},
		Spec: environmentv1alpha1.LLMWorkloadEnvironmentSpec{ClusterQueue: "development"},
	}
	landingNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        environment.Namespace,
		Annotations: map[string]string{AllowedClusterQueuesAnnotation: "production, gpu-high-memory"},
	}}
	clusterQueue := &kueuev1beta2.ClusterQueue{ObjectMeta: metav1.ObjectMeta{Name: "development"}}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&environmentv1alpha1.LLMWorkloadEnvironment{}).
		WithObjects(environment, landingNamespace, clusterQueue).
		Build()
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile unauthorized queue: %v", err)
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := client.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatalf("get environment status: %v", err)
	}
	ready := apimeta.FindStatusCondition(current.Status.Conditions, environmentv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ClusterQueueNotAllowed" {
		t.Fatalf("unexpected Ready condition: %#v", ready)
	}
	if !strings.Contains(ready.Message, "development") || !strings.Contains(ready.Message, environment.Namespace) {
		t.Fatalf("Ready condition does not explain the authorization failure: %q", ready.Message)
	}
	backingNamespace := &corev1.Namespace{}
	if err := client.Get(ctx, types.NamespacedName{Name: NamespacePrefix + environment.Name}, backingNamespace); !apierrors.IsNotFound(err) {
		t.Fatalf("backing namespace lookup error = %v, want NotFound", err)
	}
	localQueue := &kueuev1beta2.LocalQueue{}
	if err := client.Get(ctx, types.NamespacedName{Name: LocalQueueName, Namespace: NamespacePrefix + environment.Name}, localQueue); !apierrors.IsNotFound(err) {
		t.Fatalf("LocalQueue lookup error = %v, want NotFound", err)
	}
}

func TestReconcileRejectsMissingClusterQueueBeforeProvisioning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "example",
			Namespace:  "requests",
			UID:        types.UID("environment-uid"),
			Finalizers: []string{FinalizerName},
		},
		Spec: environmentv1alpha1.LLMWorkloadEnvironmentSpec{ClusterQueue: "development"},
	}
	landingNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        environment.Namespace,
		Annotations: map[string]string{AllowedClusterQueuesAnnotation: "development"},
	}}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&environmentv1alpha1.LLMWorkloadEnvironment{}).
		WithObjects(environment, landingNamespace).
		Build()
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile missing queue: %v", err)
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := client.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatalf("get environment status: %v", err)
	}
	ready := apimeta.FindStatusCondition(current.Status.Conditions, environmentv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ClusterQueueNotFound" {
		t.Fatalf("unexpected Ready condition: %#v", ready)
	}
	if !strings.Contains(ready.Message, "development") {
		t.Fatalf("Ready condition does not identify the missing queue: %q", ready.Message)
	}
	backingNamespace := &corev1.Namespace{}
	if err := client.Get(ctx, types.NamespacedName{Name: NamespacePrefix + environment.Name}, backingNamespace); !apierrors.IsNotFound(err) {
		t.Fatalf("backing namespace lookup error = %v, want NotFound", err)
	}
}

func TestNamespaceCollision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "collision",
			Namespace:  "requests",
			UID:        types.UID("environment-uid"),
			Finalizers: []string{FinalizerName},
		},
	}
	landingNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        environment.Namespace,
		Annotations: map[string]string{AllowedClusterQueuesAnnotation: DefaultClusterQueueName},
	}}
	clusterQueue := &kueuev1beta2.ClusterQueue{ObjectMeta: metav1.ObjectMeta{Name: DefaultClusterQueueName}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: NamespacePrefix + "collision"}}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&environmentv1alpha1.LLMWorkloadEnvironment{}).
		WithObjects(environment, landingNamespace, clusterQueue, namespace).
		Build()
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, Scheme: scheme}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}}); err != nil {
		t.Fatalf("reconcile collision: %v", err)
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := client.Get(ctx, types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}, current); err != nil {
		t.Fatalf("environment status: %v", err)
	}
	ready := apimeta.FindStatusCondition(current.Status.Conditions, environmentv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "NamespaceCollision" {
		t.Fatalf("unexpected Ready condition: %#v", ready)
	}
}

func TestNamespaceAccessOnlyBindsGeneratedServiceAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "revoke", Namespace: "requests", UID: types.UID("environment-uid")},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(environment).Build()
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, Scheme: scheme}
	const namespace = "env-revoke"

	if err := reconciler.ensureNamespaceServiceAccountAccess(ctx, environment, namespace); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.RoleBinding{}
	if err := client.Get(ctx, types.NamespacedName{Name: AccessBindingName, Namespace: namespace}, binding); err != nil {
		t.Fatal(err)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != NamespaceKubeconfigServiceAccountName || binding.Subjects[0].Namespace != namespace {
		t.Fatalf("unexpected subjects in environment access binding: %#v", binding.Subjects)
	}
}

func TestEnsureNamespaceKubeconfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "requests", UID: types.UID("environment-uid")},
	}
	namespace := NamespacePrefix + environment.Name
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        NamespaceKubeconfigTokenSecretName,
			Namespace:   namespace,
			Annotations: environmentOwnershipAnnotations(environment),
		},
		Data: map[string][]byte{
			corev1.ServiceAccountTokenKey:  []byte("token-value"),
			corev1.ServiceAccountRootCAKey: []byte("ca-value"),
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(environment, tokenSecret).Build()
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, Scheme: scheme, APIServerURL: "https://api.example.test/"}

	kubeconfig, ready, err := reconciler.ensureNamespaceKubeconfig(ctx, environment, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !ready || len(kubeconfig) == 0 {
		t.Fatalf("unexpected namespace kubeconfig result: ready=%v kubeconfig=%q", ready, kubeconfig)
	}
	contents := string(kubeconfig)
	for _, expected := range []string{"https://api.example.test", "token-value", "Y2EtdmFsdWU="} {
		if !strings.Contains(contents, expected) {
			t.Fatalf("kubeconfig does not contain %q: %s", expected, contents)
		}
	}
	legacySecret := &corev1.Secret{}
	if err := client.Get(ctx, types.NamespacedName{Name: "environment-example-kubeconfig", Namespace: environment.Namespace}, legacySecret); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no published kubeconfig Secret, got secret=%#v err=%v", legacySecret, err)
	}
}

func TestLocalQueueActive(t *testing.T) {
	t.Parallel()
	queue := &kueuev1beta2.LocalQueue{Status: kueuev1beta2.LocalQueueStatus{
		Conditions: []metav1.Condition{{Type: kueuev1beta2.LocalQueueActive, Status: metav1.ConditionTrue}},
	}}
	if !localQueueActive(queue) {
		t.Fatal("expected LocalQueue to be active")
	}
}

func TestClusterQueueDefaults(t *testing.T) {
	t.Parallel()
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if got := requestedClusterQueue(environment); got != kueuev1beta2.ClusterQueueReference(DefaultClusterQueueName) {
		t.Fatalf("default ClusterQueue is %q, want %q", got, DefaultClusterQueueName)
	}
}

func TestClusterQueueAllowedParsesCommaDelimitedList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		annotation string
		requested  string
		want       bool
	}{
		{name: "exact entry", annotation: "gpu-small,gpu-large", requested: "gpu-large", want: true},
		{name: "trims surrounding whitespace", annotation: "gpu-small, gpu-large ", requested: "gpu-large", want: true},
		{name: "does not match partial names", annotation: "gpu-large-extra", requested: "gpu-large", want: false},
		{name: "empty annotation denies access", annotation: "", requested: "inference-workspaces", want: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := clusterQueueAllowed(test.annotation, test.requested); got != test.want {
				t.Errorf("clusterQueueAllowed(%q, %q) = %v, want %v", test.annotation, test.requested, got, test.want)
			}
		})
	}
}

type recordingVClusterInstaller struct {
	openShift  bool
	publicHost string
	calls      int
}

func (i *recordingVClusterInstaller) Ensure(_ context.Context, _, _ string, openShift bool, publicHost string) error {
	i.openShift = openShift
	i.publicHost = publicHost
	i.calls++
	return nil
}

func TestEnsureVClusterUsesDetectedPlatformAndScopedBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "requests", UID: types.UID("environment-uid")},
		Spec:       environmentv1alpha1.LLMWorkloadEnvironmentSpec{Mode: environmentv1alpha1.EnvironmentModeVCluster},
	}
	namespace := NamespacePrefix + environment.Name
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: environment.Name, Namespace: namespace},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vc-example", Namespace: namespace},
		Data:       map[string][]byte{"config": []byte("apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: Y2E=\n    server: https://vc-example.env-example.svc\n  name: vcluster\n")},
	}
	ingress := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "config.openshift.io/v1",
		"kind":       "Ingress",
		"metadata":   map[string]interface{}{"name": "cluster"},
		"spec":       map[string]interface{}{"domain": "apps.example.test"},
	}}
	ingress.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "Ingress"})
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(environment, statefulSet, secret, ingress).Build()
	installer := &recordingVClusterInstaller{}
	reconciler := &LLMWorkloadEnvironmentReconciler{
		Client:            client,
		Scheme:            scheme,
		VClusterInstaller: installer,
		OpenShift:         true,
	}

	kubeconfig, ready, err := reconciler.ensureVCluster(ctx, environment, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !ready || len(kubeconfig) == 0 {
		t.Fatalf("unexpected vCluster readiness: ready=%v kubeconfig=%q", ready, kubeconfig)
	}
	if installer.calls != 1 || !installer.openShift || installer.publicHost != "example.apps.example.test" {
		t.Fatalf("installer did not receive OpenShift detection: %#v", installer)
	}
	binding := &rbacv1.ClusterRoleBinding{}
	if err := client.Get(ctx, types.NamespacedName{Name: "example-vcluster"}, binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Name != VClusterRoleName || len(binding.Subjects) != 1 ||
		binding.Subjects[0].Name != "vc-example" || binding.Subjects[0].Namespace != namespace {
		t.Fatalf("unexpected vCluster binding: %#v", binding)
	}
	if !environmentOwns(binding, environment) || len(binding.OwnerReferences) != 0 {
		t.Fatalf("vCluster binding has unexpected ownership metadata: annotations=%#v owners=%#v", binding.Annotations, binding.OwnerReferences)
	}
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(openShiftRouteGVK)
	if err := client.Get(ctx, types.NamespacedName{Name: "example", Namespace: namespace}, route); err != nil {
		t.Fatalf("vCluster Route: %v", err)
	}
	if host, _, _ := unstructured.NestedString(route.Object, "spec", "host"); host != "example.apps.example.test" {
		t.Fatalf("Route host = %q, want %q", host, "example.apps.example.test")
	}
	if termination, _, _ := unstructured.NestedString(route.Object, "spec", "tls", "termination"); termination != "passthrough" {
		t.Fatalf("Route TLS termination = %q, want passthrough", termination)
	}
	if !containsString(string(kubeconfig), "https://example.apps.example.test") {
		t.Fatalf("public kubeconfig server was not rewritten: %s", kubeconfig)
	}
	legacySecret := &corev1.Secret{}
	if err := client.Get(ctx, types.NamespacedName{Name: "vc-example-external", Namespace: environment.Namespace}, legacySecret); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no published vCluster kubeconfig Secret, got secret=%#v err=%v", legacySecret, err)
	}
}

func TestEnsureVClusterDoesNotCreateAccessBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "requests", UID: types.UID("environment-uid")},
		Spec:       environmentv1alpha1.LLMWorkloadEnvironmentSpec{Mode: environmentv1alpha1.EnvironmentModeVCluster},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(environment).Build()
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, Scheme: scheme}
	namespace := NamespacePrefix + environment.Name

	if err := reconciler.removeLegacyVClusterAccess(ctx, environment, namespace); err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.Role{}
	if err := client.Get(ctx, types.NamespacedName{Name: "environment-kubeconfig-reader", Namespace: namespace}, role); err == nil {
		t.Fatal("legacy vCluster access Role still exists")
	}
	binding := &rbacv1.RoleBinding{}
	if err := client.Get(ctx, types.NamespacedName{Name: AccessBindingName, Namespace: namespace}, binding); err == nil {
		t.Fatal("legacy vCluster access RoleBinding still exists")
	}
}

func TestEnvironmentRoleCannotMutateLocalQueues(t *testing.T) {
	t.Parallel()
	manifest, err := os.ReadFile("../../config/rbac/environment_role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(manifest, role); err != nil {
		t.Fatal(err)
	}
	if role.Name != EnvironmentRoleName {
		t.Fatalf("environment role is named %q, want %q", role.Name, EnvironmentRoleName)
	}

	foundLocalQueueRule := false
	for _, rule := range role.Rules {
		matchesAPIGroup := contains(rule.APIGroups, kueuev1beta2.GroupVersion.Group) || contains(rule.APIGroups, "*")
		matchesResource := contains(rule.Resources, "localqueues") || contains(rule.Resources, "*")
		if !matchesAPIGroup || !matchesResource {
			continue
		}
		foundLocalQueueRule = true
		for _, verb := range rule.Verbs {
			if verb != "get" && verb != "list" && verb != "watch" {
				t.Fatalf("environment role grants LocalQueue mutation through verb %q", verb)
			}
		}
	}
	if !foundLocalQueueRule {
		t.Fatal("environment role should grant read-only LocalQueue visibility")
	}
}

func TestVClusterRoleOnlyReadsRequiredStorageDiscovery(t *testing.T) {
	t.Parallel()
	manifest, err := os.ReadFile("../../config/rbac/vcluster_role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(manifest, role); err != nil {
		t.Fatal(err)
	}
	if role.Name != VClusterRoleName || len(role.Rules) != 1 {
		t.Fatalf("unexpected vCluster role: %#v", role)
	}
	rule := role.Rules[0]
	if len(rule.APIGroups) != 1 || rule.APIGroups[0] != "storage.k8s.io" ||
		len(rule.Resources) != 2 || !contains(rule.Resources, "csistoragecapacities") ||
		!contains(rule.Resources, "storageclasses") {
		t.Fatalf("unexpected vCluster role resources: %#v", rule)
	}
	for _, verb := range rule.Verbs {
		if verb != "get" && verb != "list" && verb != "watch" {
			t.Fatalf("vCluster role grants mutating verb %q", verb)
		}
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsString(value, target string) bool {
	return strings.Contains(value, target)
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := environmentv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kueuev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}
