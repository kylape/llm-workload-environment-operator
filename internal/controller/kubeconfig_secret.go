package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
)

// ErrKubeconfigSecretCollision is returned when an environment's kubeconfig
// Secret exists but is not controlled by that environment.
var ErrKubeconfigSecretCollision = errors.New("kubeconfig Secret is not controlled by this environment")

// kubeconfigSecretSuffix is appended to the environment name to form the
// kubeconfig Secret name. The CRD's 52-character name limit keeps the
// derived name within the 63-character object name limit.
const kubeconfigSecretSuffix = "-kubeconfig"

// KubeconfigSecretName returns the name of the Secret holding an
// environment's verified kubeconfig: <name>-kubeconfig, in the same
// namespace as the environment itself. Co-locating it with the LWE object
// keeps credential RBAC scoped to the namespaces where environments are
// created, instead of the dynamically provisioned env-* namespaces that
// hold the actual workloads.
func KubeconfigSecretName(environment *environmentv1alpha1.LLMWorkloadEnvironment) string {
	return environment.Name + kubeconfigSecretSuffix
}

// ensureKubeconfigSecret upserts the environment's kubeconfig Secret and
// returns it. It is idempotent: a Secret with unchanged data, ownership
// annotations, and controller reference is not written, so steady-state
// reconciles do not churn its ResourceVersion.
//
// Ownership is expressed both ways the operator uses elsewhere: the shared
// environment ownership annotations (for collision detection consistent
// with every other Secret this operator manages) and a controller owner
// reference (possible here because the Secret shares the environment's
// namespace), so the kubelet garbage collector reaps the Secret even if
// the environment object is removed while the operator is not running.
func (r *LLMWorkloadEnvironmentReconciler) ensureKubeconfigSecret(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	kubeconfig []byte,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KubeconfigSecretName(environment), Namespace: environment.Namespace},
		Data:       map[string][]byte{"kubeconfig": kubeconfig},
	}
	if err := r.prepareKubeconfigSecretMetadata(&secret.ObjectMeta, environment); err != nil {
		return nil, err
	}
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("creating kubeconfig Secret: %w", err)
		}
		return secret, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig Secret: %w", err)
	}
	if !environmentOwns(existing, environment) {
		return nil, ErrKubeconfigSecretCollision
	}
	if bytes.Equal(existing.Data["kubeconfig"], kubeconfig) && metav1.IsControlledBy(existing, environment) {
		return existing, nil
	}
	before := existing.DeepCopy()
	existing.Data = map[string][]byte{"kubeconfig": kubeconfig}
	if err := r.prepareKubeconfigSecretMetadata(&existing.ObjectMeta, environment); err != nil {
		return nil, err
	}
	if err := r.Patch(ctx, existing, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, fmt.Errorf("updating kubeconfig Secret: %w", err)
	}
	return existing, nil
}

// prepareKubeconfigSecretMetadata stamps the environment ownership
// annotations and controller reference onto a kubeconfig Secret object.
func (r *LLMWorkloadEnvironmentReconciler) prepareKubeconfigSecretMetadata(
	object *metav1.ObjectMeta,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
) error {
	markEnvironmentOwned(object, environment)
	if err := controllerutil.SetControllerReference(environment, object, r.Scheme); err != nil {
		return fmt.Errorf("setting kubeconfig Secret controller reference: %w", err)
	}
	return nil
}

// deleteKubeconfigSecret removes the environment's kubeconfig Secret.
// A missing Secret is not an error, so withdrawal is idempotent.
func (r *LLMWorkloadEnvironmentReconciler) deleteKubeconfigSecret(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KubeconfigSecretName(environment), Namespace: environment.Namespace},
	}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting kubeconfig Secret: %w", err)
	}
	return nil
}

// environmentKubeconfig reads the environment's verified kubeconfig from
// the Secret referenced by status. Error messages deliberately never
// include Secret data: kubeconfigs are credentials.
func (r *LLMWorkloadEnvironmentReconciler) environmentKubeconfig(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
) ([]byte, error) {
	ref := environment.Status.KubeconfigSecretRef
	if ref == nil {
		return nil, errors.New("environment has not published a kubeconfig")
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: environment.Namespace, Name: ref.Name}, secret); err != nil {
		return nil, fmt.Errorf("reading kubeconfig Secret: %w", err)
	}
	kubeconfig := secret.Data["kubeconfig"]
	if len(kubeconfig) == 0 {
		return nil, fmt.Errorf("kubeconfig Secret %s/%s has no kubeconfig data", secret.Namespace, secret.Name)
	}
	return kubeconfig, nil
}
