package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// This starts a local API server, never the management cluster's API server.
func TestHyperShiftSchema(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run API validation tests")
	}
	server := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	config, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "requests"}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		spec  LLMWorkloadEnvironmentSpec
		valid bool
	}{
		{"default", LLMWorkloadEnvironmentSpec{}, true},
		{"hyper", LLMWorkloadEnvironmentSpec{Mode: EnvironmentModeHyperShift, HyperShift: &HyperShiftEnvironmentSpec{}}, true},
		{"mode-only", LLMWorkloadEnvironmentSpec{Mode: EnvironmentModeHyperShift}, true},
		{strings.Repeat("a", 51), LLMWorkloadEnvironmentSpec{Mode: EnvironmentModeHyperShift}, true},
		{strings.Repeat("b", 52), LLMWorkloadEnvironmentSpec{Mode: EnvironmentModeHyperShift}, false},
		{"wrong-mode", LLMWorkloadEnvironmentSpec{Mode: EnvironmentModeVCluster, HyperShift: &HyperShiftEnvironmentSpec{}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := &LLMWorkloadEnvironment{ObjectMeta: metav1.ObjectMeta{Name: test.name, Namespace: "requests"}, Spec: test.spec}
			err := c.Create(ctx, e)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
			if err != nil {
				return
			}
			if test.name == "default" && e.Spec.Mode != EnvironmentModeNamespace {
				t.Fatal("legacy mode default changed")
			}
			if test.name == "hyper" && (e.Spec.HyperShift.Profile != "default" || e.Spec.HyperShift.Replicas == nil || *e.Spec.HyperShift.Replicas != 1) {
				t.Fatal("HyperShift defaults missing")
			}
			if e.Spec.Mode != EnvironmentModeHyperShift {
				return
			}
			two := int32(2)
			e.Spec.HyperShift = &HyperShiftEnvironmentSpec{Replicas: &two}
			if err := c.Update(ctx, e); err != nil {
				t.Fatalf("replicas should be mutable: %v", err)
			}
			e.Status.HostedClusterRef = &corev1.ObjectReference{APIVersion: "hypershift.openshift.io/v1beta1", Kind: "HostedCluster", Name: "cluster", Namespace: "env-test", UID: "identity"}
			if err := c.Status().Update(ctx, e); err != nil {
				t.Fatal(err)
			}
			e.Spec.HyperShift.Profile = "other"
			if err := c.Update(ctx, e); err == nil {
				t.Fatal("profile mutation accepted")
			}
		})
	}
	for _, replicas := range []int32{0, 101} {
		e := &LLMWorkloadEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "bad-count", Namespace: "requests"}, Spec: LLMWorkloadEnvironmentSpec{Mode: EnvironmentModeHyperShift, HyperShift: &HyperShiftEnvironmentSpec{Replicas: &replicas}}}
		if err := c.Create(ctx, e); err == nil {
			t.Fatalf("invalid replica count %d accepted", replicas)
		}
	}
}
