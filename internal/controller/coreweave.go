package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	VClusterAPIServiceSuffix        = "-api"
	VClusterPublicLBAnnotation      = "service.beta.kubernetes.io/coreweave-load-balancer-type"
	VClusterPublicLBAnnotationValue = "public"
)

var ErrVClusterAPIServiceCollision = errors.New("vCluster API Service already exists and is not controlled by this environment")

func (r *LLMWorkloadEnvironmentReconciler) ensureCoreWeaveVClusterAPIService(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
) (*corev1.Service, error) {
	name := environment.Name + VClusterAPIServiceSuffix
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, service); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get vCluster API Service %s/%s: %w", namespace, name, err)
	} else if err == nil && !environmentOwns(service, environment) {
		return nil, fmt.Errorf("%w: %s/%s", ErrVClusterAPIServiceCollision, namespace, name)
	}

	nodePort := vClusterAPIServiceNodePort(service)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Spec.Type = corev1.ServiceTypeLoadBalancer
		service.Spec.Selector = map[string]string{
			"app":     "vcluster",
			"release": environment.Name,
		}
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       "https",
			Protocol:   corev1.ProtocolTCP,
			Port:       443,
			TargetPort: intstr.FromInt32(8443),
			NodePort:   nodePort,
		}}
		if service.Annotations == nil {
			service.Annotations = map[string]string{}
		}
		service.Annotations[VClusterPublicLBAnnotation] = VClusterPublicLBAnnotationValue
		markEnvironmentOwned(service, environment)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ensure vCluster API LoadBalancer Service %s/%s: %w", namespace, name, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, service); err != nil {
		return nil, fmt.Errorf("read vCluster API LoadBalancer Service %s/%s: %w", namespace, name, err)
	}
	return service, nil
}

func (r *LLMWorkloadEnvironmentReconciler) removeCoreWeaveVClusterAPIService(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
) error {
	name := environment.Name + VClusterAPIServiceSuffix
	service := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, service); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get vCluster API Service %s/%s: %w", namespace, name, err)
	}
	if !environmentOwns(service, environment) {
		return fmt.Errorf("%w: %s/%s", ErrVClusterAPIServiceCollision, namespace, name)
	}
	if err := r.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete vCluster API Service %s/%s: %w", namespace, name, err)
	}
	return nil
}

func vClusterAPIServiceNodePort(service *corev1.Service) int32 {
	if service == nil {
		return 0
	}
	for _, port := range service.Spec.Ports {
		if port.Name == "https" {
			return port.NodePort
		}
	}
	return 0
}

func vClusterAPIServiceAddress(service *corev1.Service) string {
	if service == nil {
		return ""
	}
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP
		}
		if ingress.Hostname != "" {
			return ingress.Hostname
		}
	}
	return ""
}

func validateVClusterPublicAddress(address string) error {
	if address == "" || strings.TrimSpace(address) != address {
		return fmt.Errorf("vCluster API LoadBalancer must report a bare IP address or DNS name")
	}
	if net.ParseIP(address) != nil || len(validation.IsDNS1123Subdomain(address)) == 0 {
		return nil
	}
	return fmt.Errorf("invalid vCluster API LoadBalancer address %q: expected a bare IP address or DNS name without a scheme or port", address)
}

func vClusterExternalServer(address string) (string, error) {
	if err := validateVClusterPublicAddress(address); err != nil {
		return "", err
	}
	return "https://" + net.JoinHostPort(address, "443"), nil
}

func isVClusterAPIService(object client.Object) bool {
	if !strings.HasSuffix(object.GetName(), VClusterAPIServiceSuffix) {
		return false
	}
	return object.GetAnnotations()[EnvironmentOwnerAnnotation] != ""
}

func (r *LLMWorkloadEnvironmentReconciler) mapVClusterAPIService(_ context.Context, object client.Object) []reconcile.Request {
	if !isVClusterAPIService(object) {
		return nil
	}
	owner := strings.SplitN(object.GetAnnotations()[EnvironmentOwnerAnnotation], "/", 2)
	if len(owner) != 2 || owner[0] == "" || owner[1] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: owner[0], Name: owner[1]}}}
}
