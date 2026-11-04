package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ExpirationMonitor periodically expires environments whose stored absolute
// expiration timestamp has passed.
type ExpirationMonitor struct {
	Client    client.Client
	APIReader client.Reader
	Interval  time.Duration
}

func (m *ExpirationMonitor) NeedLeaderElection() bool { return true }

func (m *ExpirationMonitor) Start(ctx context.Context) error {
	interval := m.Interval
	if interval <= 0 {
		interval = ExpirationReviewInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger := log.FromContext(ctx).WithName("expiration-monitor")
	if err := m.Review(ctx, time.Now()); err != nil {
		logger.Error(err, "expiration review failed")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := m.Review(ctx, now); err != nil {
				logger.Error(err, "expiration review failed")
			}
		}
	}
}

// Review performs one expiration pass and is exposed for deterministic tests.
func (m *ExpirationMonitor) Review(ctx context.Context, now time.Time) error {
	list := &environmentv1alpha1.LLMWorkloadEnvironmentList{}
	if err := m.Client.List(ctx, list); err != nil {
		return fmt.Errorf("list LLM workload environments: %w", err)
	}

	var reviewErr error
	for index := range list.Items {
		environment := &list.Items[index]
		if environment.DeletionTimestamp != nil {
			continue
		}
		if environment.Status.ExpirationTimestamp != nil {
			if err := refreshExpirationRemaining(ctx, m.Client, m.apiReader(), environment, now); err != nil {
				reviewErr = errors.Join(reviewErr, fmt.Errorf("refresh expiration for LWE %s/%s: %w", environment.Namespace, environment.Name, err))
			}
		}
		if !isEnvironmentExpired(environment, now) && !environmentExpiredCondition(environment) {
			continue
		}
		if expirationExempt(environment) && !environmentExpiredCondition(environment) {
			continue
		}
		namespaceName := NamespacePrefix + environment.Name
		if environment.Status.NamespaceRef != nil && environment.Status.NamespaceRef.Name != "" {
			namespaceName = environment.Status.NamespaceRef.Name
		}
		if err := markEnvironmentExpired(ctx, m.Client, m.apiReader(), environment, namespaceName, now); err != nil {
			reviewErr = errors.Join(reviewErr, fmt.Errorf("expire LWE %s/%s: %w", environment.Namespace, environment.Name, err))
		}
	}
	return reviewErr
}

func (m *ExpirationMonitor) apiReader() client.Reader {
	if m.APIReader != nil {
		return m.APIReader
	}
	return m.Client
}

func (r *LLMWorkloadEnvironmentReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func expirationRemaining(now, expiresAt time.Time) string {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return "0s"
	}

	minutes := int64(remaining / time.Minute)
	if remaining%time.Minute != 0 {
		minutes++
	}
	hours := minutes / 60
	minutes %= 60
	if hours == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	if minutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, minutes)
}

func refreshExpirationRemaining(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	now time.Time,
) error {
	if writer == nil || reader == nil {
		return fmt.Errorf("expiration monitor requires a Kubernetes client and reader")
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &environmentv1alpha1.LLMWorkloadEnvironment{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: environment.Namespace, Name: environment.Name}, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.DeletionTimestamp != nil || current.Status.ExpirationTimestamp == nil {
			return nil
		}
		remaining := expirationRemaining(now, current.Status.ExpirationTimestamp.Time)
		if current.Status.ExpirationRemaining == remaining {
			return nil
		}
		before := current.DeepCopy()
		current.Status.ExpirationRemaining = remaining
		if err := writer.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		environment.Status = current.Status
		return nil
	})
}

func expirationExempt(environment *environmentv1alpha1.LLMWorkloadEnvironment) bool {
	return environment.Annotations[ExpirationExemptAnnotation] == "true"
}

func isEnvironmentExpired(environment *environmentv1alpha1.LLMWorkloadEnvironment, now time.Time) bool {
	return environment.Status.ExpirationTimestamp != nil && !now.Before(environment.Status.ExpirationTimestamp.Time)
}

func environmentExpiredCondition(environment *environmentv1alpha1.LLMWorkloadEnvironment) bool {
	for _, condition := range environment.Status.Conditions {
		if condition.Type == environmentv1alpha1.ExpiredCondition && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// markEnvironmentExpired first commits the terminal status condition, then
// deletes the owned namespace. The periodic monitor retries namespace deletion
// if the API request fails after status was updated.
func markEnvironmentExpired(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespaceName string,
	now time.Time,
) error {
	if now.IsZero() {
		now = time.Now()
	}
	if writer == nil || reader == nil {
		return fmt.Errorf("expiration monitor requires a Kubernetes client and reader")
	}

	namespace := &corev1.Namespace{}
	err := reader.Get(ctx, types.NamespacedName{Name: namespaceName}, namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get namespace %q: %w", namespaceName, err)
	}
	if err == nil && !environmentOwns(namespace, environment) {
		return fmt.Errorf("namespace %q is not controlled by this LWE", namespaceName)
	}

	shouldDeleteNamespace := false
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		shouldDeleteNamespace = false
		current := &environmentv1alpha1.LLMWorkloadEnvironment{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: environment.Namespace, Name: environment.Name}, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.DeletionTimestamp != nil {
			return nil
		}
		if !environmentExpiredCondition(current) && (!isEnvironmentExpired(current, now) || expirationExempt(current)) {
			return nil
		}
		shouldDeleteNamespace = true
		before := current.DeepCopy()
		apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: environmentv1alpha1.ExpiredCondition, Status: metav1.ConditionTrue,
			ObservedGeneration: current.Generation, Reason: "ExpirationReached",
			Message: "Environment expiration time has passed; its namespace is being removed",
		})
		apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: environmentv1alpha1.ReadyCondition, Status: metav1.ConditionFalse,
			ObservedGeneration: current.Generation, Reason: "Expired", Message: "Environment has expired",
		})
		apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: environmentv1alpha1.ExpirationReadyCondition, Status: metav1.ConditionTrue,
			ObservedGeneration: current.Generation, Reason: "Expired", Message: "Environment lifetime has ended",
		})
		current.Status.ExpirationRemaining = "0s"
		if err := writer.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		environment.Status = current.Status
		return nil
	}); err != nil {
		return fmt.Errorf("update expired status: %w", err)
	}
	if !shouldDeleteNamespace {
		return nil
	}

	if apierrors.IsNotFound(err) {
		return nil
	}
	if namespace.DeletionTimestamp != nil {
		return nil
	}
	uid := namespace.UID
	if deleteErr := writer.Delete(ctx, namespace, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
		return fmt.Errorf("delete namespace %q: %w", namespaceName, deleteErr)
	}
	return nil
}
