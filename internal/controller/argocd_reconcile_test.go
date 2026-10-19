package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	clusterplatform "github.com/kylape/llm-workload-environment-operator/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

type readyArgoCDTestKueue struct{}

func (readyArgoCDTestKueue) EnsureClusterQueueExists(context.Context, string) error { return nil }
func (readyArgoCDTestKueue) EnsureLocalQueue(context.Context, *environmentv1alpha1.LLMWorkloadEnvironment, string, string) (bool, error) {
	return true, nil
}

func argoCDReconcileFixture(t *testing.T) (*LLMWorkloadEnvironmentReconciler, *environmentv1alpha1.LLMWorkloadEnvironment, ctrl.Request) {
	t.Helper()
	environment := newArgoCDTestEnvironment(t)
	environment.Status = environmentv1alpha1.LLMWorkloadEnvironmentStatus{}
	landing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: environment.Namespace, Annotations: map[string]string{AllowedClusterQueuesAnnotation: DefaultClusterQueueName}}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: NamespacePrefix + environment.Name, Annotations: environmentOwnershipAnnotations(environment)}}
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: environment.Name, Namespace: namespace.Name},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, CurrentRevision: "revision", UpdateRevision: "revision"},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: environment.Name + VClusterAPIServiceSuffix, Namespace: namespace.Name, Annotations: environmentOwnershipAnnotations(environment)},
		Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.10"}}}},
	}
	config := testArgoCDKubeconfig()
	config.Clusters["public"].Server = "https://internal-api.env-example.svc"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vc-" + environment.Name, Namespace: namespace.Name}, Data: map[string][]byte{"config": encodeArgoCDTestKubeconfig(t, config)}}
	c := argoCDTestClient(t, environment, landing, namespace, statefulSet, service, secret)
	r := &LLMWorkloadEnvironmentReconciler{
		Client: c, Scheme: testScheme(t), Kueue: readyArgoCDTestKueue{}, Platform: clusterplatform.PlatformCoreWeave,
		VClusterInstaller: &recordingVClusterInstaller{}, ArgoCDNamespace: "argocd",
	}
	return r, environment, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(environment)}
}

func requireArgoCDCondition(t *testing.T, environment *environmentv1alpha1.LLMWorkloadEnvironment, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := apimeta.FindStatusCondition(environment.Status.Conditions, environmentv1alpha1.ArgoCDRegisteredCondition)
	if condition == nil || condition.Status != status || condition.Reason != reason {
		t.Fatalf("unexpected registration condition: %#v", condition)
	}
}

func TestReconcileArgoCDUsesFinalizedPublicKubeconfig(t *testing.T) {
	ctx := context.Background()
	r, environment, request := argoCDReconcileFixture(t)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	if !apimeta.IsStatusConditionTrue(environment.Status.Conditions, environmentv1alpha1.ReadyCondition) {
		t.Fatal("provisioning did not become ready")
	}
	requireArgoCDCondition(t, environment, metav1.ConditionTrue, "Registered")
	secret := &corev1.Secret{}
	key := client.ObjectKey{Name: argoCDClusterSecretName(environment), Namespace: "argocd"}
	if err := r.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["server"]) != "https://203.0.113.10:443" || strings.Contains(string(environment.Status.Kubeconfig), "internal-api") {
		t.Fatal("registration did not use the finalized public kubeconfig")
	}
	var connection argoCDClusterConfig
	if err := json.Unmarshal(secret.Data["config"], &connection); err != nil {
		t.Fatal(err)
	}
	if connection.TLSClientConfig.Insecure {
		t.Fatal("registration disabled TLS verification")
	}
	version, secretVersion := environment.ResourceVersion, secret.ResourceVersion
	result, err := r.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	if err := r.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	if environment.ResourceVersion != version || secret.ResourceVersion != secretVersion {
		t.Fatal("steady-state reconcile wrote status or credentials unnecessarily")
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Fatal("active registration has no watch recovery resync")
	}
}

func TestReconcileArgoCDFailurePreservesEnvironmentReadiness(t *testing.T) {
	ctx := context.Background()
	r, environment, request := argoCDReconcileFixture(t)
	underlying := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(underlying, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if secret, ok := obj.(*corev1.Secret); ok && secret.Namespace == "argocd" {
			return errors.New("private-tenant-token")
		}
		return c.Create(ctx, obj, opts...)
	}})
	if _, err := r.Reconcile(ctx, request); err == nil || strings.Contains(err.Error(), "private-tenant-token") {
		t.Fatal("registration failure was not returned safely")
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	if !apimeta.IsStatusConditionTrue(environment.Status.Conditions, environmentv1alpha1.ReadyCondition) || len(environment.Status.Kubeconfig) == 0 {
		t.Fatal("optional registration failure cleared provisioning readiness or credentials")
	}
	requireArgoCDCondition(t, environment, metav1.ConditionFalse, "RegistrationFailed")
	// Retrying through a new reconciler uses the durable intent left by failure.
	r.Client = underlying
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	requireArgoCDCondition(t, environment, metav1.ConditionTrue, "Registered")
}

func TestReconcileArgoCDOptOutWithRevokedQueueAndReoptIn(t *testing.T) {
	ctx := context.Background()
	r, environment, request := argoCDReconcileFixture(t)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	name := environment.Status.ArgoCDClusterSecretRef.Name
	generation := environment.Generation
	delete(environment.Labels, ApplicationProfileLabel)
	if err := r.Update(ctx, environment); err != nil {
		t.Fatal(err)
	}
	if environment.Generation != generation {
		t.Fatal("test must exercise a metadata-only change")
	}
	landing := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: environment.Namespace}, landing); err != nil {
		t.Fatal(err)
	}
	landing.Annotations[AllowedClusterQueuesAnnotation] = ""
	if err := r.Update(ctx, landing); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	requireArgoCDCondition(t, environment, metav1.ConditionFalse, "NotRequested")
	ready := apimeta.FindStatusCondition(environment.Status.Conditions, environmentv1alpha1.ReadyCondition)
	if ready == nil || ready.Reason != "ClusterQueueNotAllowed" {
		t.Fatal("queue rejection no longer reported")
	}
	if environment.Status.ArgoCDClusterSecretRef != nil {
		t.Fatal("opt-out retained the registration reference")
	}
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: "argocd"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatal("opt-out did not delete registration")
	}
	if err := r.Get(ctx, client.ObjectKey{Name: NamespacePrefix + environment.Name}, &corev1.Namespace{}); err != nil {
		t.Fatal("opt-out deleted the environment")
	}
	landing.Annotations[AllowedClusterQueuesAnnotation] = DefaultClusterQueueName
	if err := r.Update(ctx, landing); err != nil {
		t.Fatal(err)
	}
	environment.Labels[ApplicationProfileLabel] = "demo"
	if err := r.Update(ctx, environment); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	requireArgoCDCondition(t, environment, metav1.ConditionTrue, "Registered")
	if environment.Status.ArgoCDClusterSecretRef.Name != name {
		t.Fatal("re-opt-in changed cluster identity")
	}
}

func TestReconcileArgoCDRetainsRegistrationWhileEnvironmentUnavailable(t *testing.T) {
	ctx := context.Background()
	r, environment, request := argoCDReconcileFixture(t)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	statefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Name: environment.Name, Namespace: NamespacePrefix + environment.Name}, statefulSet); err != nil {
		t.Fatal(err)
	}
	statefulSet.Status.ReadyReplicas = 0
	if err := r.Status().Update(ctx, statefulSet); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	requireArgoCDCondition(t, environment, metav1.ConditionUnknown, "EnvironmentUnavailable")
	if err := r.Get(ctx, client.ObjectKey{Name: argoCDClusterSecretName(environment), Namespace: "argocd"}, &corev1.Secret{}); err != nil {
		t.Fatal("temporary unavailability removed registration")
	}
}

func TestReconcileArgoCDCleanupRetriesBeforeClearingReference(t *testing.T) {
	ctx := context.Background()
	r, environment, request := argoCDReconcileFixture(t)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	delete(environment.Labels, ApplicationProfileLabel)
	if err := r.Update(ctx, environment); err != nil {
		t.Fatal(err)
	}
	underlying := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(underlying, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		return errors.New("private-tenant-token")
	}})
	if _, err := r.Reconcile(ctx, request); err == nil || strings.Contains(err.Error(), "private-tenant-token") {
		t.Fatal("cleanup API failure not safely retried")
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	if environment.Status.ArgoCDClusterSecretRef == nil {
		t.Fatal("failed cleanup lost its reference")
	}
	requireArgoCDCondition(t, environment, metav1.ConditionFalse, "RegistrationFailed")
	r.Client = underlying
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	requireArgoCDCondition(t, environment, metav1.ConditionFalse, "NotRequested")
}

func TestReconcileArgoCDFinalizationBeforeEnvironmentDeletion(t *testing.T) {
	ctx := context.Background()
	r, environment, request := argoCDReconcileFixture(t)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	if err := r.Delete(ctx, environment); err != nil {
		t.Fatal(err)
	}
	// Disabling the integration after registration must not skip finalization.
	r.ArgoCDNamespace = ""
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKey{Name: NamespacePrefix + environment.Name}, &corev1.Namespace{}); err != nil {
		t.Fatal("backing namespace was deleted before registration absence was confirmed")
	}
	if err := r.Get(ctx, client.ObjectKey{Name: argoCDClusterSecretName(environment), Namespace: "argocd"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatal("registration not removed first")
	}
	for i := 0; i < 6; i++ {
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		if err := r.Get(ctx, request.NamespacedName, &environmentv1alpha1.LLMWorkloadEnvironment{}); apierrors.IsNotFound(err) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("finalizer did not finish")
}

func TestArgoCDOperatorRBACAlreadySupportsSecretLifecycle(t *testing.T) {
	raw, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
		allowed := false
		for _, rule := range role.Rules {
			if containsStringSlice(rule.APIGroups, "") && containsStringSlice(rule.Resources, "secrets") && containsStringSlice(rule.Verbs, verb) && len(rule.ResourceNames) == 0 {
				allowed = true
			}
		}
		if !allowed {
			t.Fatalf("operator lacks Secret %s permission", verb)
		}
	}
}

func containsStringSlice(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func TestReconcileArgoCDPendingCleanupDoesNotHideStatusFailure(t *testing.T) {
	ctx := context.Background()
	r, environment, request := argoCDReconcileFixture(t)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	delete(environment.Labels, ApplicationProfileLabel)
	if err := r.Update(ctx, environment); err != nil {
		t.Fatal(err)
	}
	underlying := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(underlying, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, resource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if e, ok := obj.(*environmentv1alpha1.LLMWorkloadEnvironment); ok {
				condition := apimeta.FindStatusCondition(e.Status.Conditions, environmentv1alpha1.ArgoCDRegisteredCondition)
				if condition != nil && condition.Reason == "Unregistering" {
					return errors.New("private-tenant-token")
				}
			}
			return c.SubResource(resource).Patch(ctx, obj, patch, opts...)
		},
	})
	if _, err := r.Reconcile(ctx, request); err == nil || strings.Contains(err.Error(), "private-tenant-token") {
		t.Fatal("a failed status write must remain a sanitized retryable error")
	}
	r.Client = underlying
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	requireArgoCDCondition(t, environment, metav1.ConditionFalse, "NotRequested")
}
