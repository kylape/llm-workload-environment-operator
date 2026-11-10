package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
)

// ErrGatewayBindingCollision is returned when a gateway access RoleBinding
// exists but is not controlled by this environment.
var ErrGatewayBindingCollision = errors.New("gateway access RoleBinding already exists and is not controlled by this environment")

const (
	// GatewayAccessRoleName is the ClusterRole bound to environment credentials
	// in the shared gateway namespaces.
	GatewayAccessRoleName = "llm-workload-environment-gateway-user"
	// GatewayAccessBindingName is the name of the RoleBinding the operator
	// creates for each environment in every configured gateway namespace.
	GatewayAccessBindingName = "environment-gateway-access"
)

// DefaultGatewayNamespaces are the namespaces that hold the inference gateway
// infrastructure on the supported platforms: openshift-ingress on OpenShift
// (RHOAI attaches routes to openshift-ai-inference there) and
// redhat-ods-applications on xKS (inference-gateway-istio). Overridable with
// the GATEWAY_NAMESPACES environment variable.
var DefaultGatewayNamespaces = []string{"openshift-ingress", "redhat-ods-applications"}

// ensureGatewayAccess binds the environment ServiceAccount to
// GatewayAccessRoleName in every configured gateway namespace so environment
// credentials can resolve and reach the Gateway an LLMInferenceService routes
// through (get services and Gateways, port-forward to gateway pods).
//
// Gateway namespaces that do not exist on the cluster are skipped, so a
// platform without a given namespace does not block reconciliation.
func (r *LLMWorkloadEnvironmentReconciler) ensureGatewayAccess(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespaceName string,
) error {
	for _, gatewayNamespace := range r.gatewayNamespaces() {
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: gatewayNamespace}, ns); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get gateway namespace %q: %w", gatewayNamespace, err)
		}

		binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: GatewayAccessBindingName, Namespace: gatewayNamespace}}
		if err := r.Get(ctx, types.NamespacedName{Name: GatewayAccessBindingName, Namespace: gatewayNamespace}, binding); err != nil && !apierrors.IsNotFound(err) {
			return err
		} else if err == nil && !environmentOwns(binding, environment) {
			return fmt.Errorf("%w: %s/%s", ErrGatewayBindingCollision, gatewayNamespace, GatewayAccessBindingName)
		}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
			binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: GatewayAccessRoleName}
			binding.Subjects = []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      NamespaceKubeconfigServiceAccountName,
				Namespace: namespaceName,
			}}
			markEnvironmentOwned(binding, environment)
			return nil
		})
		if err != nil {
			return fmt.Errorf("ensure gateway access RoleBinding in %q: %w", gatewayNamespace, err)
		}
	}
	return nil
}

// removeGatewayAccess deletes the environment's gateway access RoleBindings.
// Unlike the namespace-scoped access binding, these live in namespaces the
// environment does not own, so they must be removed explicitly.
func (r *LLMWorkloadEnvironmentReconciler) removeGatewayAccess(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
) error {
	for _, gatewayNamespace := range r.gatewayNamespaces() {
		binding := &rbacv1.RoleBinding{}
		if err := r.Get(ctx, types.NamespacedName{Name: GatewayAccessBindingName, Namespace: gatewayNamespace}, binding); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if environmentOwns(binding, environment) {
			if err := r.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *LLMWorkloadEnvironmentReconciler) gatewayNamespaces() []string {
	if len(r.GatewayNamespaces) == 0 {
		return DefaultGatewayNamespaces
	}
	return r.GatewayNamespaces
}
