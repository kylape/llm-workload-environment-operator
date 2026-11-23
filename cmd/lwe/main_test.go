package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// withFakeClient swaps the CLI's client factory for a fake client built
// from the given objects, restoring it when the test ends.
func withFakeClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	original := newClientFor
	newClientFor = func(string) (client.Client, error) { return c, nil }
	t.Cleanup(func() { newClientFor = original })
	return c
}

func readyEnvironment(name string) *environmentv1alpha1.LLMWorkloadEnvironment {
	return &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "requests",
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Spec: environmentv1alpha1.LLMWorkloadEnvironmentSpec{Mode: environmentv1alpha1.EnvironmentModeVCluster},
		Status: environmentv1alpha1.LLMWorkloadEnvironmentStatus{
			KubeconfigSecretRef: &corev1.LocalObjectReference{Name: name + "-kubeconfig"},
			Conditions: []metav1.Condition{{
				Type: environmentv1alpha1.ReadyCondition, Status: metav1.ConditionTrue,
				Reason: "Ready", Message: "Environment is ready", LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func kubeconfigSecret(name string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-kubeconfig", Namespace: "requests"},
		Data:       map[string][]byte{"kubeconfig": data},
	}
}

func TestUpAdoptsReadyEnvironmentAndVerifiesCredentials(t *testing.T) {
	environment := readyEnvironment("adopted")
	c := withFakeClient(t, environment, kubeconfigSecret("adopted", []byte("kubeconfig-bytes")))

	if err := runUp(context.Background(), []string{"-namespace", "requests", "-mode", "VCluster", "-name", "adopted"}); err != nil {
		t.Fatal(err)
	}
	// runUp prints the name to stdout; the fake-client adoption path is
	// asserted through the environment object below.
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "requests", Name: "adopted"}, current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Mode != environmentv1alpha1.EnvironmentModeVCluster {
		t.Fatal("adoption changed the environment mode")
	}
}

func TestUpWaitsForEnvironmentToBecomeReady(t *testing.T) {
	c := withFakeClient(t)

	// Simulate the operator: after creation, publish readiness and
	// credentials asynchronously — the object runUp first sees is not
	// yet ready, so its post-wait credential read must refresh.
	go func() {
		time.Sleep(100 * time.Millisecond)
		key := client.ObjectKey{Namespace: "requests", Name: "created"}
		current := &environmentv1alpha1.LLMWorkloadEnvironment{}
		if err := c.Get(context.Background(), key, current); err != nil {
			return
		}
		current.Status = readyEnvironment("created").Status
		if err := c.Status().Update(context.Background(), current); err != nil {
			return
		}
		if err := c.Create(context.Background(), kubeconfigSecret("created", []byte("async-bytes"))); err != nil {
			return
		}
	}()

	if err := runUp(context.Background(), []string{"-namespace", "requests", "-mode", "VCluster", "-name", "created", "-timeout", "30s"}); err != nil {
		t.Fatal(err)
	}
}

func TestUpRejectsModeMismatch(t *testing.T) {
	environment := readyEnvironment("existing")
	environment.Spec.Mode = environmentv1alpha1.EnvironmentModeVCluster
	withFakeClient(t, environment, kubeconfigSecret("existing", []byte("kubeconfig-bytes")))

	err := runUp(context.Background(), []string{"-namespace", "requests", "-mode", "Namespace", "-name", "existing"})
	if err == nil || !strings.Contains(err.Error(), "exists with mode") {
		t.Fatalf("expected a mode mismatch error, got %v", err)
	}
}

func TestUpCreatesLabeledEnvironment(t *testing.T) {
	c := withFakeClient(t)

	// The wait times out immediately (no reconciler drives the object to
	// readiness in a unit test); the assertion is the created object.
	err := runUp(context.Background(), []string{
		"-namespace", "requests", "-mode", "VCluster", "-name", "created",
		"-duration", "1h", "-label", "ci-run=42", "-timeout", "1ms",
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a wait timeout, got %v", err)
	}
	created := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "requests", Name: "created"}, created); err != nil {
		t.Fatal(err)
	}
	if created.Labels[managedByLabel] != managedByValue || created.Labels["ci-run"] != "42" {
		t.Fatalf("environment misses CLI or caller labels: %#v", created.Labels)
	}
	if created.Spec.Mode != environmentv1alpha1.EnvironmentModeVCluster || created.Spec.Duration != "1h" {
		t.Fatalf("environment misses spec values: %#v", created.Spec)
	}
}

func TestReadKubeconfigPrefersSecretReference(t *testing.T) {
	environment := readyEnvironment("secret-reader")
	environment.Status.Kubeconfig = []byte("legacy-bytes") //nolint:staticcheck // SA1019: asserting the new path wins over the deprecated field
	c := withFakeClient(t, environment, kubeconfigSecret("secret-reader", []byte("secret-bytes")))

	kubeconfig, err := readKubeconfig(context.Background(), c, environment)
	if err != nil {
		t.Fatal(err)
	}
	if string(kubeconfig) != "secret-bytes" {
		t.Fatalf("expected Secret data, got %q", kubeconfig)
	}
}

func TestReadKubeconfigFallsBackToInlineField(t *testing.T) {
	environment := readyEnvironment("legacy")
	environment.Status.KubeconfigSecretRef = nil
	environment.Status.Kubeconfig = []byte("legacy-bytes") //nolint:staticcheck // SA1019: asserting the migration fallback
	c := withFakeClient(t, environment)

	kubeconfig, err := readKubeconfig(context.Background(), c, environment)
	if err != nil {
		t.Fatal(err)
	}
	if string(kubeconfig) != "legacy-bytes" {
		t.Fatalf("expected the inline fallback, got %q", kubeconfig)
	}
}

func TestReadKubeconfigRequiresPublication(t *testing.T) {
	environment := readyEnvironment("unpublished")
	environment.Status.KubeconfigSecretRef = nil
	environment.Status.Kubeconfig = nil //nolint:staticcheck // SA1019: clearing the deprecated field for the negative case
	c := withFakeClient(t, environment)

	if _, err := readKubeconfig(context.Background(), c, environment); err == nil {
		t.Fatal("expected an error for an environment without credentials")
	}
}

func TestKubeconfigCommandWritesFile(t *testing.T) {
	environment := readyEnvironment("writer")
	withFakeClient(t, environment, kubeconfigSecret("writer", []byte("file-bytes")))

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := runKubeconfig(context.Background(), []string{"-namespace", "requests", "-output", path, "writer"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig file mode is %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "file-bytes" {
		t.Fatalf("unexpected kubeconfig content %q", data)
	}
}

func TestDownIsIdempotent(t *testing.T) {
	withFakeClient(t)
	if err := runDown(context.Background(), []string{"-namespace", "requests", "gone"}); err != nil {
		t.Fatalf("deleting a missing environment must succeed, got %v", err)
	}
}

func TestDownDeletesEnvironment(t *testing.T) {
	environment := readyEnvironment("doomed")
	c := withFakeClient(t, environment, kubeconfigSecret("doomed", []byte("kubeconfig-bytes")))

	if err := runDown(context.Background(), []string{"-namespace", "requests", "doomed"}); err != nil {
		t.Fatal(err)
	}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "requests", Name: "doomed"}, environment)
	if err == nil {
		t.Fatal("environment was not deleted")
	}
}

func TestSweepHonorsAgeAndSelector(t *testing.T) {
	old := readyEnvironment("old")
	old.CreationTimestamp = metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
	fresh := readyEnvironment("fresh")
	fresh.CreationTimestamp = metav1.Time{Time: time.Now().Add(-1 * time.Minute)}
	foreign := readyEnvironment("foreign")
	foreign.Labels = map[string]string{"owner": "someone-else"}
	foreign.CreationTimestamp = metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
	c := withFakeClient(t, old, fresh, foreign, kubeconfigSecret("old", []byte("k")), kubeconfigSecret("fresh", []byte("k")), kubeconfigSecret("foreign", []byte("k")))

	if err := runSweep(context.Background(), []string{"-namespace", "requests", "-older-than", "1h"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old"} {
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "requests", Name: name}, &environmentv1alpha1.LLMWorkloadEnvironment{}); err == nil {
			t.Fatalf("%s should have been swept", name)
		}
	}
	for _, name := range []string{"fresh", "foreign"} {
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "requests", Name: name}, &environmentv1alpha1.LLMWorkloadEnvironment{}); err != nil {
			t.Fatalf("%s should have survived the sweep", name)
		}
	}
}

func TestSweepDryRunDeletesNothing(t *testing.T) {
	old := readyEnvironment("old")
	old.CreationTimestamp = metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
	c := withFakeClient(t, old, kubeconfigSecret("old", []byte("k")))

	if err := runSweep(context.Background(), []string{"-namespace", "requests", "-older-than", "1h", "-dry-run"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "requests", Name: "old"}, &environmentv1alpha1.LLMWorkloadEnvironment{}); err != nil {
		t.Fatal("dry-run deleted an environment")
	}
}

func TestWaitReadyFailsOnExpiredEnvironment(t *testing.T) {
	environment := readyEnvironment("expired")
	apimeta.SetStatusCondition(&environment.Status.Conditions, metav1.Condition{
		Type: environmentv1alpha1.ExpiredCondition, Status: metav1.ConditionTrue,
		Reason: "Expired", Message: "duration exhausted", LastTransitionTime: metav1.Now(),
	})
	c := withFakeClient(t, environment)

	err := waitReady(context.Background(), c, environment)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected an expiration error, got %v", err)
	}
}

func TestLabelsFlag(t *testing.T) {
	parsed := labelsFlag{}
	if err := parsed.Set("ci-run=42"); err != nil {
		t.Fatal(err)
	}
	if err := parsed.Set("no-separator"); err == nil {
		t.Fatal("malformed label was accepted")
	}
	if parsed.String() != "ci-run=42" {
		t.Fatalf("unexpected labels %q", parsed.String())
	}
}
