package controller

import (
	"context"
	"testing"
	"time"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExpirationConfigFromEnv(t *testing.T) {
	for _, test := range []struct {
		name    string
		values  map[string]string
		wantMin time.Duration
		wantMax time.Duration
		invalid bool
	}{
		{name: "defaults", wantMin: 15 * time.Minute, wantMax: 7 * 24 * time.Hour},
		{name: "custom", values: map[string]string{"LWE_MIN_DURATION": "30m", "LWE_MAX_DURATION": "5h30m"}, wantMin: 30 * time.Minute, wantMax: 330 * time.Minute},
		{name: "invalid minimum", values: map[string]string{"LWE_MIN_DURATION": "later"}, invalid: true},
		{name: "zero maximum", values: map[string]string{"LWE_MAX_DURATION": "0s"}, invalid: true},
		{name: "minimum over maximum", values: map[string]string{"LWE_MIN_DURATION": "2h", "LWE_MAX_DURATION": "1h"}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := ExpirationConfigFromEnv(func(key string) string { return test.values[key] })
			if (err != nil) != test.invalid {
				t.Fatalf("ExpirationConfigFromEnv() error = %v, invalid=%t", err, test.invalid)
			}
			if err == nil && (config.Minimum != test.wantMin || config.Maximum != test.wantMax) {
				t.Fatalf("got range %s to %s, want %s to %s", config.Minimum, config.Maximum, test.wantMin, test.wantMax)
			}
		})
	}
}

func TestReconcileExpiration(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	oldExpiry := metav1.NewTime(time.Now().Add(2 * time.Hour))
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "requests"},
		Spec:       environmentv1alpha1.LLMWorkloadEnvironmentSpec{Duration: "5h30m"},
		Status: environmentv1alpha1.LLMWorkloadEnvironmentStatus{
			ExpirationTimestamp:  &oldExpiry,
			LastObservedDuration: "2h",
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(environment).
		WithObjects(environment).
		Build()
	config := ExpirationConfig{Minimum: time.Hour, Maximum: 24 * time.Hour}
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, APIReader: client, Expiration: &config}

	invalid, err := reconciler.reconcileExpiration(ctx, environment)
	if err != nil || invalid {
		t.Fatalf("valid duration update failed: invalid=%t err=%v", invalid, err)
	}
	if environment.Status.ExpirationTimestamp == nil || time.Until(environment.Status.ExpirationTimestamp.Time) < 5*time.Hour {
		t.Fatalf("valid update did not set a new absolute deadline: %#v", environment.Status.ExpirationTimestamp)
	}
	if environment.Status.LastObservedDuration != "5h30m" {
		t.Fatalf("last observed duration = %q", environment.Status.LastObservedDuration)
	}

	validExpiry := environment.Status.ExpirationTimestamp.DeepCopy()
	environment.Spec.Duration = "31d"
	if err := client.Update(ctx, environment); err != nil {
		t.Fatalf("update invalid requested duration: %v", err)
	}
	invalid, err = reconciler.reconcileExpiration(ctx, environment)
	if err != nil || !invalid {
		t.Fatalf("invalid duration was not rejected: invalid=%t err=%v", invalid, err)
	}
	if !environment.Status.ExpirationTimestamp.Equal(validExpiry) {
		t.Fatalf("invalid update changed expiration timestamp: got %s, want %s", environment.Status.ExpirationTimestamp, validExpiry)
	}
	if condition := apimeta.FindStatusCondition(environment.Status.Conditions, environmentv1alpha1.ExpirationReadyCondition); condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("invalid duration did not set ExpirationReady=False: %#v", condition)
	}
}

func TestInvalidInitialDurationPreventsNamespaceCreation(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "invalid", Namespace: "requests", UID: types.UID("environment-uid"),
			Finalizers: []string{FinalizerName},
		},
		Spec: environmentv1alpha1.LLMWorkloadEnvironmentSpec{Duration: "not-a-duration"},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(environment).
		WithObjects(environment).
		Build()
	config := ExpirationConfig{Minimum: time.Hour, Maximum: 24 * time.Hour}
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, APIReader: client, Expiration: &config}
	if _, err := reconciler.reconcileEnvironment(ctx, environment, NamespacePrefix+environment.Name); err != nil {
		t.Fatalf("reconcile invalid initial duration: %v", err)
	}
	if err := client.Get(ctx, types.NamespacedName{Name: NamespacePrefix + environment.Name}, &corev1.Namespace{}); err == nil {
		t.Fatal("namespace was created for an invalid initial duration")
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: environment.Namespace, Name: environment.Name}, current); err != nil {
		t.Fatalf("get LWE: %v", err)
	}
	if condition := apimeta.FindStatusCondition(current.Status.Conditions, environmentv1alpha1.ReadyCondition); condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "InvalidDuration" {
		t.Fatalf("invalid initial duration did not set Ready=False: %#v", condition)
	}
}

func TestExpirationMonitorDeletesExpiredNamespaceAndMarksLWE(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	expiredAt := metav1.NewTime(time.Now().Add(-time.Minute))
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "requests", UID: types.UID("environment-uid")},
		Status:     environmentv1alpha1.LLMWorkloadEnvironmentStatus{ExpirationTimestamp: &expiredAt},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "env-expired", UID: types.UID("namespace-uid"),
		Annotations: environmentOwnershipAnnotations(environment),
	}}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(environment).
		WithObjects(environment, namespace).
		Build()
	monitor := &ExpirationMonitor{Client: client, APIReader: client}
	if err := monitor.Review(ctx, time.Now()); err != nil {
		t.Fatalf("review expired LWE: %v", err)
	}
	if err := client.Get(ctx, types.NamespacedName{Name: namespace.Name}, &corev1.Namespace{}); err != nil {
		t.Fatal("expiration monitor removed the namespace before mode-specific cleanup", err)
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: environment.Namespace, Name: environment.Name}, current); err != nil {
		t.Fatalf("get LWE: %v", err)
	}
	if !apimeta.IsStatusConditionTrue(current.Status.Conditions, environmentv1alpha1.ExpiredCondition) {
		t.Fatalf("LWE is not marked expired: %#v", current.Status.Conditions)
	}
	if condition := apimeta.FindStatusCondition(current.Status.Conditions, environmentv1alpha1.ReadyCondition); condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "Expired" {
		t.Fatalf("unexpected Ready condition after expiration: %#v", condition)
	}
	reconciler := &LLMWorkloadEnvironmentReconciler{Client: client, APIReader: client}
	if _, err := reconciler.cleanupEnvironmentResources(ctx, current, namespace.Name); err != nil {
		t.Fatalf("clean up expired environment: %v", err)
	}
	if err := client.Get(ctx, types.NamespacedName{Name: namespace.Name}, &corev1.Namespace{}); err == nil {
		t.Fatal("expired namespace was not removed after mode cleanup")
	}
}

func TestExpirationMonitorHonorsAdminExemption(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	expiredAt := metav1.NewTime(time.Now().Add(-time.Minute))
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "exempt", Namespace: "requests", UID: types.UID("environment-uid"),
			Annotations: map[string]string{ExpirationExemptAnnotation: "true"},
		},
		Status: environmentv1alpha1.LLMWorkloadEnvironmentStatus{ExpirationTimestamp: &expiredAt},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "env-exempt", UID: types.UID("namespace-uid"),
		Annotations: environmentOwnershipAnnotations(environment),
	}}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(environment).
		WithObjects(environment, namespace).
		Build()
	monitor := &ExpirationMonitor{Client: client, APIReader: client}
	if err := monitor.Review(ctx, time.Now()); err != nil {
		t.Fatalf("review exempt LWE: %v", err)
	}
	if err := client.Get(ctx, types.NamespacedName{Name: namespace.Name}, &corev1.Namespace{}); err != nil {
		t.Fatalf("exempt namespace was deleted: %v", err)
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: environment.Namespace, Name: environment.Name}, current); err != nil {
		t.Fatalf("get LWE: %v", err)
	}
	if environmentExpiredCondition(current) {
		t.Fatal("exempt LWE was marked expired")
	}
}
