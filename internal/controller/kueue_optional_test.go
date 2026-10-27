package controller

import (
	"context"
	"reflect"
	"testing"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	clusterplatform "github.com/kylape/llm-workload-environment-operator/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// No ClusterQueue, LocalQueue, or Kueue ConfigMap is installed in this fixture.
func optionalKueueFixture(t *testing.T, mode environmentv1alpha1.EnvironmentMode) (*LLMWorkloadEnvironmentReconciler, *environmentv1alpha1.LLMWorkloadEnvironment, ctrl.Request) {
	t.Helper()
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "optional", Namespace: "requests", UID: types.UID("optional-uid"), Finalizers: []string{FinalizerName}},
		Spec:       environmentv1alpha1.LLMWorkloadEnvironmentSpec{Mode: mode, ClusterQueue: "not-installed"},
	}
	namespace := NamespacePrefix + environment.Name
	objects := []client.Object{environment, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: environment.Namespace}}}
	if mode == environmentv1alpha1.EnvironmentModeNamespace {
		objects = append(objects, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: NamespaceKubeconfigTokenSecretName, Namespace: namespace, Annotations: environmentOwnershipAnnotations(environment)},
			Data:       map[string][]byte{corev1.ServiceAccountTokenKey: []byte("test-token"), corev1.ServiceAccountRootCAKey: []byte("test-ca")},
		})
	} else {
		objects = append(objects,
			&appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: environment.Name, Namespace: namespace},
				Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, CurrentRevision: "ready", UpdateRevision: "ready"},
			},
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: environment.Name + VClusterAPIServiceSuffix, Namespace: namespace, Annotations: environmentOwnershipAnnotations(environment)},
				Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.10"}}}},
			},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vc-" + environment.Name, Namespace: namespace}, Data: map[string][]byte{"config": encodeArgoCDTestKubeconfig(t, testArgoCDKubeconfig())}},
		)
	}
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(environment).WithObjects(objects...).Build()
	config := DefaultKueueAdmissionConfig()
	config.Enabled = false
	r := &LLMWorkloadEnvironmentReconciler{
		Client: c, Scheme: scheme, KueueAdmission: &config, APIServerURL: "https://host.example.test",
		Platform: clusterplatform.PlatformCoreWeave, VClusterInstaller: &recordingVClusterInstaller{},
	}
	return r, environment, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(environment)}
}

// Catch accidental reads or writes even if their errors would be ignored.
func forbidKueueAccess(t *testing.T, r *LLMWorkloadEnvironmentReconciler) {
	t.Helper()
	check := func(obj runtime.Object) {
		gvks, _, err := r.Scheme.ObjectKinds(obj)
		if err != nil {
			t.Fatal(err)
		}
		for _, gvk := range gvks {
			if gvk.Group == kueueAPIGroup || gvk.Kind == "ConfigMap" {
				t.Errorf("unexpected Kueue resource access: %s", gvk)
			}
		}
	}
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			check(obj)
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			check(list)
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			check(obj)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			check(obj)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			check(obj)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			check(obj)
			return c.Delete(ctx, obj, opts...)
		},
	})
}

func TestReconcileWithoutKueue(t *testing.T) {
	for _, mode := range []environmentv1alpha1.EnvironmentMode{environmentv1alpha1.EnvironmentModeNamespace, environmentv1alpha1.EnvironmentModeVCluster} {
		t.Run(string(mode), func(t *testing.T) {
			r, environment, request := optionalKueueFixture(t, mode)
			forbidKueueAccess(t, r)
			result, err := r.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			environment = loadArgoCDTestEnvironment(t, r.Client, environment)
			if !apimeta.IsStatusConditionTrue(environment.Status.Conditions, environmentv1alpha1.ReadyCondition) || len(environment.Status.Kubeconfig) == 0 {
				t.Fatalf("environment failed without Kueue: %#v", environment.Status.Conditions)
			}
			if result.RequeueAfter != 0 {
				t.Fatalf("disabled integration requested policy polling: %#v", result)
			}
			ns := &corev1.Namespace{}
			if err := r.Get(context.Background(), client.ObjectKey{Name: NamespacePrefix + environment.Name}, ns); err != nil {
				t.Fatal(err)
			}
			if _, found := ns.Labels[KueueManagedLabel]; found || ns.Annotations[kueueNamespaceLabelsAnnotation] != "" {
				t.Fatal("disabled integration applied Kueue namespace metadata")
			}
			// Re-enabling restores both allowlist enforcement and API dependency.
			r.KueueAdmission.Enabled = true
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			environment = loadArgoCDTestEnvironment(t, r.Client, environment)
			if condition := apimeta.FindStatusCondition(environment.Status.Conditions, environmentv1alpha1.ReadyCondition); condition.Reason != "ClusterQueueNotAllowed" {
				t.Fatalf("queue policy not restored: %#v", condition)
			}
			landing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: environment.Namespace}}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(landing), landing); err != nil {
				t.Fatal(err)
			}
			landing.Annotations = map[string]string{AllowedClusterQueuesAnnotation: environment.Spec.ClusterQueue}
			if err := r.Update(context.Background(), landing); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			environment = loadArgoCDTestEnvironment(t, r.Client, environment)
			if condition := apimeta.FindStatusCondition(environment.Status.Conditions, environmentv1alpha1.ReadyCondition); condition.Reason != "KueueAPINotAvailable" {
				t.Fatalf("missing API accepted after re-enable: %#v", condition)
			}
		})
	}
}

func TestDisabledKueueLeavesExistingPolicyUntouched(t *testing.T) {
	r, environment, request := optionalKueueFixture(t, environmentv1alpha1.EnvironmentModeNamespace)
	base := r.Client
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: NamespacePrefix + environment.Name, Labels: map[string]string{KueueManagedLabel: "true"}, Annotations: environmentOwnershipAnnotations(environment),
	}}
	namespace.Annotations[kueueNamespaceLabelsAnnotation] = "invalid-legacy-record"
	queue := newKueueObject(schema.GroupVersion{Group: kueueAPIGroup, Version: kueueV1Beta2}, "LocalQueue", LocalQueueName, namespace.Name)
	for _, object := range []client.Object{namespace, queue} {
		if err := base.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	before := queue.DeepCopy()
	forbidKueueAccess(t, r)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	if !apimeta.IsStatusConditionTrue(environment.Status.Conditions, environmentv1alpha1.ReadyCondition) {
		t.Fatalf("legacy Kueue state blocked opt-out: %#v", environment.Status.Conditions)
	}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(queue), queue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, queue) {
		t.Fatal("disabled integration changed the existing LocalQueue")
	}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(namespace), namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels[KueueManagedLabel] != "true" || namespace.Annotations[kueueNamespaceLabelsAnnotation] != "invalid-legacy-record" {
		t.Fatal("disabled integration changed existing Kueue namespace policy")
	}
}

type trackingKueue struct {
	checks, applies int
	active          bool
}

func (k *trackingKueue) EnsureClusterQueueExists(context.Context, string) error {
	k.checks++
	return nil
}

func (k *trackingKueue) EnsureLocalQueue(context.Context, *environmentv1alpha1.LLMWorkloadEnvironment, string, string) (bool, error) {
	k.applies++
	return k.active, nil
}

func TestValidationOptOutStillRequiresAndAppliesQueues(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "inactive", true: "active"}[active], func(t *testing.T) {
			r, environment, request := optionalKueueFixture(t, environmentv1alpha1.EnvironmentModeNamespace)
			r.KueueAdmission.Enabled = true
			r.KueueAdmission.ValidationEnabled = false
			queue := &trackingKueue{active: active}
			r.Kueue = queue
			landing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: environment.Namespace}}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(landing), landing); err != nil {
				t.Fatal(err)
			}
			landing.Annotations = map[string]string{AllowedClusterQueuesAnnotation: environment.Spec.ClusterQueue}
			if err := r.Update(context.Background(), landing); err != nil {
				t.Fatal(err)
			}
			forbidKueueAccess(t, r)
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			environment = loadArgoCDTestEnvironment(t, r.Client, environment)
			if apimeta.IsStatusConditionTrue(environment.Status.Conditions, environmentv1alpha1.ReadyCondition) != active {
				t.Fatalf("queue readiness ignored: %#v", environment.Status.Conditions)
			}
			if queue.checks != 1 || queue.applies != 1 {
				t.Fatalf("queue integration skipped: checks=%d, applies=%d", queue.checks, queue.applies)
			}
			ns := &corev1.Namespace{}
			if err := r.Get(context.Background(), client.ObjectKey{Name: NamespacePrefix + environment.Name}, ns); err != nil {
				t.Fatal(err)
			}
			if ns.Labels[KueueManagedLabel] != "true" {
				t.Fatal("validation opt-out skipped Kueue namespace labels")
			}
		})
	}
}
