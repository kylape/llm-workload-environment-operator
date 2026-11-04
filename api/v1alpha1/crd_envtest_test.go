package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// This runs a local apiserver/etcd pair, never the user's host cluster.
func TestCRDSNODefaultsAndImmutableOptions(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS or run make test-api")
	}
	controlPlane := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	config, err := controlPlane.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := controlPlane.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err = corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err = environmentv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sno-api-tests"}}); err != nil {
		t.Fatal(err)
	}
	object := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "sno-defaults", Namespace: "sno-api-tests"},
		Spec:       environmentv1alpha1.LLMWorkloadEnvironmentSpec{Mode: environmentv1alpha1.EnvironmentModeSNO, SNO: &environmentv1alpha1.SNOEnvironmentSpec{}},
	}
	if err = c.Create(ctx, object); err != nil {
		t.Fatal(err)
	}
	if object.Spec.SNO.ImageProfile != "default" {
		t.Fatal("provider profile default was not applied")
	}
	changed := object.DeepCopy()
	changed.Spec.SNO.ImageProfile = "different"
	if err = c.Update(ctx, changed); !apierrors.IsInvalid(err) {
		t.Fatalf("provider options were not immutable: %v", err)
	}
	changed = object.DeepCopy()
	changed.Spec.Mode = environmentv1alpha1.EnvironmentModeVCluster
	if err = c.Update(ctx, changed); !apierrors.IsInvalid(err) {
		t.Fatalf("SNO mode was not immutable: %v", err)
	}
	// Status observations must remain writable while spec is immutable.
	object.Status.SNOVirtualMachineRef = &corev1.ObjectReference{APIVersion: "sno.klape.io/v1alpha1", Kind: "SNOVirtualMachine", Name: "sno", Namespace: "env-sno-defaults"}
	if err = c.Status().Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	legacy := &environmentv1alpha1.LLMWorkloadEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "legacy-default", Namespace: "sno-api-tests"}}
	if err = c.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Spec.Mode != environmentv1alpha1.EnvironmentModeNamespace {
		t.Fatal("legacy default mode changed")
	}
}
