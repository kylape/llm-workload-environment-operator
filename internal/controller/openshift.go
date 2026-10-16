package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	clusterplatform "github.com/kylape/llm-workload-environment-operator/internal/platform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var ErrRouteCollision = errors.New("vCluster Route already exists and is not controlled by this environment")

var openShiftRouteGVK = schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
var openShiftIngressGVK = schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "Ingress"}
var openShiftInfrastructureGVK = schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "Infrastructure"}

func (r *LLMWorkloadEnvironmentReconciler) isOpenShift() bool {
	return r.Platform == clusterplatform.PlatformOpenShift || (r.Platform == "" && r.OpenShift)
}

func (r *LLMWorkloadEnvironmentReconciler) openShiftAPIServerURL(ctx context.Context) (string, bool, error) {
	infrastructure := &unstructured.Unstructured{}
	infrastructure.SetGroupVersionKind(openShiftInfrastructureGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: "cluster"}, infrastructure); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	url, found, err := unstructured.NestedString(infrastructure.Object, "status", "apiServerURL")
	if err != nil {
		return "", false, err
	}
	if !found || url == "" {
		return "", false, nil
	}
	return strings.TrimRight(url, "/"), true, nil
}

func (r *LLMWorkloadEnvironmentReconciler) ensureOpenShiftVClusterEndpoint(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
	kubeconfig []byte,
) ([]byte, error) {
	host, err := r.openShiftVClusterHost(ctx, environment)
	if err != nil {
		return nil, err
	}

	if err := r.ensureOpenShiftRoute(ctx, environment, namespace, host); err != nil {
		return nil, err
	}

	rewrittenKubeconfig, err := rewriteKubeconfigServer(kubeconfig, "https://"+host)
	if err != nil {
		return nil, fmt.Errorf("rewrite vCluster kubeconfig for Route %s: %w", host, err)
	}
	return rewrittenKubeconfig, nil
}

func (r *LLMWorkloadEnvironmentReconciler) openShiftVClusterHost(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
) (string, error) {
	appsDomain, err := r.openShiftAppsDomain(ctx)
	if err != nil {
		return "", fmt.Errorf("discover OpenShift applications domain: %w", err)
	}
	return environment.Name + "." + appsDomain, nil
}

func (r *LLMWorkloadEnvironmentReconciler) openShiftAppsDomain(ctx context.Context) (string, error) {
	ingress := &unstructured.Unstructured{}
	ingress.SetGroupVersionKind(openShiftIngressGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: "cluster"}, ingress); err != nil {
		return "", err
	}
	if domain, found, err := unstructured.NestedString(ingress.Object, "spec", "domain"); err != nil {
		return "", err
	} else if found && domain != "" {
		return domain, nil
	}
	if domain, found, err := unstructured.NestedString(ingress.Object, "status", "domain"); err != nil {
		return "", err
	} else if found && domain != "" {
		return domain, nil
	}
	return "", errors.New("OpenShift Ingress object does not publish an applications domain")
}

func (r *LLMWorkloadEnvironmentReconciler) ensureOpenShiftRoute(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
	host string,
) error {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(openShiftRouteGVK)
	routeKey := types.NamespacedName{Name: environment.Name, Namespace: namespace}
	if err := r.Get(ctx, routeKey, route); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get vCluster Route: %w", err)
		}
		route = desiredOpenShiftRoute(environment.Name, namespace, host)
		markEnvironmentOwned(route, environment)
		if err := r.Create(ctx, route); err != nil {
			return fmt.Errorf("create vCluster Route: %w", err)
		}
		return nil
	}
	if !environmentOwns(route, environment) {
		return fmt.Errorf("%w: %s/%s", ErrRouteCollision, namespace, environment.Name)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Object["spec"] = desiredOpenShiftRoute(environment.Name, namespace, host).Object["spec"]
		markEnvironmentOwned(route, environment)
		return nil
	})
	if err != nil {
		return fmt.Errorf("update vCluster Route: %w", err)
	}
	return nil
}

func desiredOpenShiftRoute(name, namespace, host string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": openShiftRouteGVK.Group + "/" + openShiftRouteGVK.Version,
		"kind":       openShiftRouteGVK.Kind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"host": host,
			"to": map[string]interface{}{
				"kind":   "Service",
				"name":   name,
				"weight": int64(100),
			},
			"port": map[string]interface{}{
				"targetPort": "https",
			},
			"tls": map[string]interface{}{
				"termination": "passthrough",
			},
		},
	}}
}
