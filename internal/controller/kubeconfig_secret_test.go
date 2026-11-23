package controller

import (
	"context"
	"errors"
	"testing"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newKubeconfigSecretTestEnvironment returns a minimal environment with no
// published credentials.
func newKubeconfigSecretTestEnvironment() *environmentv1alpha1.LLMWorkloadEnvironment {
	return &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "requests", UID: types.UID("environment-uid")},
		Spec:       environmentv1alpha1.LLMWorkloadEnvironmentSpec{Mode: environmentv1alpha1.EnvironmentModeNamespace},
	}
}

func newKubeconfigSecretTestClient(t *testing.T, environment *environmentv1alpha1.LLMWorkloadEnvironment, objects ...client.Object) client.WithWatch {
	t.Helper()
	objects = append(objects, environment)
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(environment).WithObjects(objects...).Build()
}

func loadKubeconfigSecretTestEnvironment(t *testing.T, c client.Client, environment *environmentv1alpha1.LLMWorkloadEnvironment) *environmentv1alpha1.LLMWorkloadEnvironment {
	t.Helper()
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(environment), current); err != nil {
		t.Fatal(err)
	}
	return current
}

func TestSetReadyPublishesAndWithdrawsKubeconfigSecret(t *testing.T) {
	ctx := context.Background()
	environment := newKubeconfigSecretTestEnvironment()
	c := newKubeconfigSecretTestClient(t, environment)
	r := &LLMWorkloadEnvironmentReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.setReady(ctx, environment, "env-example", []byte("kubeconfig-bytes"), metav1.ConditionTrue, "Ready", "Environment is ready"); err != nil {
		t.Fatal(err)
	}
	environment = loadKubeconfigSecretTestEnvironment(t, c, environment)
	if environment.Status.KubeconfigSecretRef == nil || environment.Status.KubeconfigSecretRef.Name != KubeconfigSecretName(environment) {
		t.Fatalf("ready environment does not reference its kubeconfig Secret: %#v", environment.Status.KubeconfigSecretRef)
	}
	if string(environment.Status.Kubeconfig) != "kubeconfig-bytes" { //nolint:staticcheck // SA1019: asserts the migration window keeps the legacy field populated
		t.Fatal("legacy inline kubeconfig was not populated during the migration window")
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: environment.Namespace, Name: KubeconfigSecretName(environment)}
	if err := c.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["kubeconfig"]) != "kubeconfig-bytes" || !metav1.IsControlledBy(secret, environment) || !environmentOwns(secret, environment) {
		t.Fatal("kubeconfig Secret has wrong data, controller reference, or ownership annotations")
	}

	// Steady state: republishing identical credentials must not rewrite the
	// Secret (ResourceVersion is the fake client's write witness).
	version := secret.ResourceVersion
	if err := r.setReady(ctx, environment, "env-example", []byte("kubeconfig-bytes"), metav1.ConditionTrue, "Ready", "Environment is ready"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	if secret.ResourceVersion != version {
		t.Fatal("unchanged kubeconfig Secret was rewritten")
	}

	// Rotation: changed credentials update the Secret in place.
	if err := r.setReady(ctx, environment, "env-example", []byte("rotated-bytes"), metav1.ConditionTrue, "Ready", "Environment is ready"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["kubeconfig"]) != "rotated-bytes" {
		t.Fatal("rotated kubeconfig Secret was not updated")
	}

	// Withdrawal: a not-ready reconcile clears the Secret and the reference,
	// mirroring the inline field.
	if err := r.setReady(ctx, environment, "env-example", nil, metav1.ConditionFalse, "NotReady", "credentials withdrawn"); err != nil {
		t.Fatal(err)
	}
	environment = loadKubeconfigSecretTestEnvironment(t, c, environment)
	if environment.Status.KubeconfigSecretRef != nil || len(environment.Status.Kubeconfig) != 0 { //nolint:staticcheck // SA1019: asserts withdrawal clears the legacy field too
		t.Fatal("not-ready environment kept its credentials")
	}
	if err := c.Get(ctx, key, secret); !apierrors.IsNotFound(err) {
		t.Fatal("kubeconfig Secret was not withdrawn")
	}
}

func TestSetReadyRejectsKubeconfigSecretCollision(t *testing.T) {
	ctx := context.Background()
	environment := newKubeconfigSecretTestEnvironment()
	// A Secret occupying the derived name without this environment's
	// ownership annotations models a foreign object.
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: KubeconfigSecretName(environment), Namespace: environment.Namespace}}
	c := newKubeconfigSecretTestClient(t, environment, foreign)
	r := &LLMWorkloadEnvironmentReconciler{Client: c, Scheme: testScheme(t)}

	err := r.setReady(ctx, environment, "env-example", []byte("kubeconfig-bytes"), metav1.ConditionTrue, "Ready", "Environment is ready")
	if !errors.Is(err, ErrKubeconfigSecretCollision) {
		t.Fatalf("expected a collision error, got %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil || len(foreign.Data) != 0 {
		t.Fatal("foreign Secret was modified or removed")
	}
}
