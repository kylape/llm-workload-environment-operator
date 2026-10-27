package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testKueueAdmissionYAML = `apiVersion: config.kueue.x-k8s.io/v1beta1
kind: Configuration
managedJobsNamespaceSelector:
  matchLabels:
    kueue.openshift.io/managed: "true"
integrations:
  frameworks: [pod]
`

func testKueueAdmissionConfigMap() *corev1.ConfigMap {
	c := DefaultKueueAdmissionConfig()
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: c.ConfigNamespace, Name: c.ConfigName},
		Data:       map[string]string{c.ConfigKey: testKueueAdmissionYAML},
	}
}

func TestKueueAdmissionEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name               string
		env                map[string]string
		invalid            bool
		disabled           bool
		validationDisabled bool
	}{
		{name: "defaults"},
		{name: "custom", env: map[string]string{
			"KUEUE_NAMESPACE_LABELS": `{"kueue.x-k8s.io/managed":"true","team":"inference"}`,
			"KUEUE_CONFIG_REF":       "custom-kueue:settings:config.yaml",
		}},
		{name: "disabled", env: map[string]string{"KUEUE_ADMISSION_ENABLED": "false"}, disabled: true},
		{name: "validation disabled", env: map[string]string{"KUEUE_ADMISSION_VALIDATION_ENABLED": "false"}, validationDisabled: true},
		{name: "both disabled", env: map[string]string{"KUEUE_ADMISSION_ENABLED": "false", "KUEUE_ADMISSION_VALIDATION_ENABLED": "false"}, disabled: true, validationDisabled: true},
		{name: "empty labels", env: map[string]string{"KUEUE_NAMESPACE_LABELS": `{}`}},
		{name: "invalid boolean", env: map[string]string{"KUEUE_ADMISSION_ENABLED": "maybe"}, invalid: true},
		{name: "invalid validation boolean", env: map[string]string{"KUEUE_ADMISSION_VALIDATION_ENABLED": "maybe"}, invalid: true},
		{name: "invalid json", env: map[string]string{"KUEUE_NAMESPACE_LABELS": `managed=true`}, invalid: true},
		{name: "null labels", env: map[string]string{"KUEUE_NAMESPACE_LABELS": `null`}, invalid: true},
		{name: "non-string labels", env: map[string]string{"KUEUE_NAMESPACE_LABELS": `{"managed":true}`}, invalid: true},
		{name: "null label value", env: map[string]string{"KUEUE_NAMESPACE_LABELS": `{"managed":null}`}, invalid: true},
		{name: "invalid label key", env: map[string]string{"KUEUE_NAMESPACE_LABELS": `{"bad key":"true"}`}, invalid: true},
		{name: "invalid label value", env: map[string]string{"KUEUE_NAMESPACE_LABELS": `{"managed":"bad value"}`}, invalid: true},
		{name: "reserved label", env: map[string]string{"KUEUE_NAMESPACE_LABELS": `{"llm.redhat.com/environment":"other"}`}, invalid: true},
		{name: "invalid namespace", env: map[string]string{"KUEUE_CONFIG_REF": "Bad:settings:config.yaml"}, invalid: true},
		{name: "invalid config name", env: map[string]string{"KUEUE_CONFIG_REF": "kueue-system:bad/name:config.yaml"}, invalid: true},
		{name: "invalid config key", env: map[string]string{"KUEUE_CONFIG_REF": "kueue-system:settings:../config"}, invalid: true},
		{name: "missing part", env: map[string]string{"KUEUE_CONFIG_REF": "kueue-system:settings"}, invalid: true},
		{name: "extra part", env: map[string]string{"KUEUE_CONFIG_REF": "kueue-system:settings:config.yaml:extra"}, invalid: true},
		{name: "empty namespace", env: map[string]string{"KUEUE_CONFIG_REF": ":settings:config.yaml"}, invalid: true},
		{name: "empty name", env: map[string]string{"KUEUE_CONFIG_REF": "kueue-system::config.yaml"}, invalid: true},
		{name: "empty key", env: map[string]string{"KUEUE_CONFIG_REF": "kueue-system:settings:"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := KueueAdmissionConfigFromEnv(func(key string) string { return tc.env[key] })
			if (err != nil) != tc.invalid {
				t.Fatalf("config error: %v", err)
			}
			if tc.invalid {
				return
			}
			if c.Enabled != !tc.disabled || c.ValidationEnabled != !tc.validationDisabled {
				t.Fatalf("Enabled=%v, ValidationEnabled=%v", c.Enabled, c.ValidationEnabled)
			}
			if tc.name == "custom" {
				if len(c.NamespaceLabels) != 2 || c.NamespaceLabels["kueue.x-k8s.io/managed"] != "true" || c.ConfigNamespace != "custom-kueue" || c.ConfigName != "settings" || c.ConfigKey != "config.yaml" {
					t.Fatalf("override did not replace defaults: %#v", c)
				}
			}
			if tc.name == "empty labels" && len(c.NamespaceLabels) != 0 {
				t.Fatal("default label was retained")
			}
			if tc.name == "defaults" && (c.ConfigNamespace != "kueue-system" || c.ConfigName != "kueue-manager-config" || c.ConfigKey != "controller_manager_config.yaml") {
				t.Fatalf("unexpected default reference: %#v", c)
			}
		})
	}
}

func TestKueuePodNamespaceSelector(t *testing.T) {
	for _, tc := range []struct {
		name, config     string
		invalid, matches bool
	}{
		{name: "modern match", config: testKueueAdmissionYAML, matches: true},
		{name: "modern beta2", config: strings.ReplaceAll(testKueueAdmissionYAML, "v1beta1", "v1beta2"), matches: true},
		{name: "legacy", config: `apiVersion: config.kueue.x-k8s.io/v1beta1
kind: Configuration
integrations:
  frameworks: [pod]
  podOptions:
    namespaceSelector:
      matchExpressions:
        - key: kueue.openshift.io/managed
          operator: In
          values: ["true"]
`, matches: true},
		{name: "modern takes precedence", config: testKueueAdmissionYAML + `  podOptions:
    namespaceSelector:
      matchLabels:
        other: required
`, matches: true},
		{name: "all conditions evaluated", config: `apiVersion: config.kueue.x-k8s.io/v1beta2
kind: Configuration
integrations:
  frameworks: [pod]
managedJobsNamespaceSelector:
  matchLabels:
    kueue.openshift.io/managed: "true"
  matchExpressions:
    - key: team
      operator: Exists
`},
		{name: "namespace mismatch", config: strings.ReplaceAll(testKueueAdmissionYAML, "kueue.openshift.io", "kueue.x-k8s.io")},
		{name: "invalid yaml", config: "[", invalid: true},
		{name: "missing data", config: "", invalid: true},
		{name: "wrong kind", config: strings.ReplaceAll(testKueueAdmissionYAML, "Configuration", "ConfigMap"), invalid: true},
		{name: "unsupported version", config: strings.ReplaceAll(testKueueAdmissionYAML, "v1beta1", "v9"), invalid: true},
		{name: "no pod integration", config: strings.ReplaceAll(testKueueAdmissionYAML, "[pod]", "[batch/job]"), invalid: true},
		{name: "implicit selector", config: "apiVersion: config.kueue.x-k8s.io/v1beta2\nkind: Configuration\nintegrations:\n  frameworks: [pod]\n", invalid: true},
		{name: "invalid expression", config: "apiVersion: config.kueue.x-k8s.io/v1beta2\nkind: Configuration\nintegrations:\n  frameworks: [pod]\nmanagedJobsNamespaceSelector:\n  matchExpressions:\n    - key: managed\n      operator: Invalid\n", invalid: true},
		{name: "invalid selector label", config: strings.ReplaceAll(testKueueAdmissionYAML, "kueue.openshift.io/managed", "bad key"), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector, err := kueuePodNamespaceSelector([]byte(tc.config))
			if (err != nil) != tc.invalid {
				t.Fatalf("selector error: %v", err)
			}
			if err == nil && selector.Matches(labels.Set{KueueManagedLabel: "true"}) != tc.matches {
				t.Fatalf("unexpected match for %s", selector)
			}
		})
	}
}

func TestKueueNamespaceLabelMigrationAndRemoval(t *testing.T) {
	c := DefaultKueueAdmissionConfig()
	c.NamespaceLabels = map[string]string{"kueue.x-k8s.io/managed": "true"}
	r := &LLMWorkloadEnvironmentReconciler{KueueAdmission: &c}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{KueueManagedLabel: "true", "team": "demo"}}}
	if err := r.applyKueueNamespaceLabels(ns); err != nil {
		t.Fatal(err)
	}
	if _, old := ns.Labels[KueueManagedLabel]; old || ns.Labels["kueue.x-k8s.io/managed"] != "true" || ns.Labels["team"] != "demo" {
		t.Fatalf("migration: %#v", ns.Labels)
	}
	c.NamespaceLabels = map[string]string{}
	if err := r.applyKueueNamespaceLabels(ns); err != nil {
		t.Fatal(err)
	}
	if len(ns.Labels) != 1 || ns.Labels["team"] != "demo" {
		t.Fatalf("removal: %#v", ns.Labels)
	}
	// A retired key edited by an administrator must not be deleted.
	c.NamespaceLabels = map[string]string{"kueue.x-k8s.io/managed": "true"}
	if err := r.applyKueueNamespaceLabels(ns); err != nil {
		t.Fatal(err)
	}
	ns.Labels["kueue.x-k8s.io/managed"] = "admin-value"
	c.NamespaceLabels = map[string]string{}
	if err := r.applyKueueNamespaceLabels(ns); err != nil {
		t.Fatal(err)
	}
	if ns.Labels["kueue.x-k8s.io/managed"] != "admin-value" {
		t.Fatal("administrator edit was removed")
	}
	ns.Annotations[kueueNamespaceLabelsAnnotation] = `{"llm.redhat.com/environment":"example"}`
	if err := r.applyKueueNamespaceLabels(ns); err == nil {
		t.Fatal("reserved labels in migration record were accepted")
	}
}

func TestKueueNamespaceLabelsRespectIntegrationSwitch(t *testing.T) {
	config := DefaultKueueAdmissionConfig()
	config.Enabled = false
	r := &LLMWorkloadEnvironmentReconciler{KueueAdmission: &config}
	for _, ns := range []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "new-namespace"}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{KueueManagedLabel: "true"}, Annotations: map[string]string{kueueNamespaceLabelsAnnotation: "invalid-record"}}},
	} {
		before := ns.DeepCopy()
		if err := r.applyKueueNamespaceLabels(ns); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ns, before) {
			t.Fatalf("disabled integration changed namespace: %#v", ns)
		}
	}
	config.Enabled = true
	config.ValidationEnabled = false
	ns := &corev1.Namespace{}
	if err := r.applyKueueNamespaceLabels(ns); err != nil {
		t.Fatal(err)
	}
	if ns.Labels[KueueManagedLabel] != "true" {
		t.Fatal("disabling validation must not disable namespace labels")
	}
}

func TestKueueAdmissionUsesConfiguredReferenceAndLiveReader(t *testing.T) {
	ctx := context.Background()
	config := DefaultKueueAdmissionConfig()
	config.ConfigNamespace, config.ConfigName, config.ConfigKey = "custom", "policy", "settings.yaml"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "env-example", Labels: map[string]string{KueueManagedLabel: "true"}}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigName, Namespace: config.ConfigNamespace}, Data: map[string]string{config.ConfigKey: testKueueAdmissionYAML}}
	live := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ns, cm).Build()
	// A stale cache lacks both objects; validation must use APIReader.
	r := &LLMWorkloadEnvironmentReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), APIReader: live, KueueAdmission: &config}
	if reason, err := r.validateKueueAdmission(ctx, ns.Name); reason != "" || err != nil {
		t.Fatalf("live validation failed: %s %v", reason, err)
	}
	delete(cm.Data, config.ConfigKey)
	if err := live.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if reason, err := r.validateKueueAdmission(ctx, ns.Name); reason != "KueueAdmissionConfigurationInvalid" || err == nil {
		t.Fatalf("missing configured data key was accepted: %s %v", reason, err)
	}
}

func TestKueueAdmissionReadFailuresAndDisabled(t *testing.T) {
	ctx := context.Background()
	for _, forbidden := range []bool{false, true} {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if forbidden {
					return apierrors.NewForbidden(corev1.Resource("configmaps"), key.Name, nil)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
		r := &LLMWorkloadEnvironmentReconciler{Client: c}
		if reason, err := r.validateKueueAdmission(ctx, "env-example"); reason != "KueueAdmissionValidationFailed" || err == nil {
			t.Fatalf("missing/forbidden must fail closed: %s %v", reason, err)
		}
		config := DefaultKueueAdmissionConfig()
		config.ValidationEnabled = false
		r.KueueAdmission = &config
		if reason, err := r.validateKueueAdmission(ctx, "env-example"); reason != "" || err != nil {
			t.Fatalf("disabled validation performed reads: %s %v", reason, err)
		}
	}
}

func TestReconcileKueueAdmissionMismatchRecoveryAndDrift(t *testing.T) {
	ctx := context.Background()
	r, environment, request := argoCDReconcileFixture(t)
	config := DefaultKueueAdmissionConfig()
	config.NamespaceLabels = map[string]string{"kueue.x-k8s.io/managed": "true"}
	r.KueueAdmission = &config
	// The configured labels conflict with the selector in the ConfigMap.
	result, err := r.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	ready := apimeta.FindStatusCondition(environment.Status.Conditions, environmentv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "KueueNamespaceNotSelected" || result.RequeueAfter != 30*time.Second {
		t.Fatalf("mismatch: %#v, result=%#v", ready, result)
	}
	if environment.Status.ArgoCDClusterSecretRef != nil {
		t.Fatal("unselected environment registered with Argo CD")
	}
	// Correct the labels and verify provisioning/registration can finish.
	config.NamespaceLabels = map[string]string{KueueManagedLabel: "true"}
	result, err = r.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	if !apimeta.IsStatusConditionTrue(environment.Status.Conditions, environmentv1alpha1.ReadyCondition) || result.RequeueAfter != 5*time.Minute {
		t.Fatalf("recovery failed: %#v", environment.Status.Conditions)
	}
	// Configuration changes must revoke Ready on subsequent checks.
	cm := testKueueAdmissionConfigMap()
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatal(err)
	}
	cm.Data[config.ConfigKey] = strings.ReplaceAll(testKueueAdmissionYAML, "kueue.openshift.io", "kueue.x-k8s.io")
	if err := r.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	ready = apimeta.FindStatusCondition(environment.Status.Conditions, environmentv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "KueueNamespaceNotSelected" {
		t.Fatalf("drift ignored: %#v", ready)
	}
	// Explicit administrator opt-out restores readiness without reading config.
	config.ValidationEnabled = false
	if err := r.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, r.Client, environment)
	if !apimeta.IsStatusConditionTrue(environment.Status.Conditions, environmentv1alpha1.ReadyCondition) {
		t.Fatalf("opt-out failed: %#v", environment.Status.Conditions)
	}
}
