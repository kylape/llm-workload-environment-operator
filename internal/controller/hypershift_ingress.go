package controller

import (
	"context"
	"fmt"

	envapi "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var hostedControlPlaneGVK = schema.GroupVersionKind{Group: "hypershift.openshift.io", Version: "v1beta1", Kind: "HostedControlPlane"}

func (r *LLMWorkloadEnvironmentReconciler) ensureHyperShiftAPIRoute(ctx context.Context, e *envapi.LLMWorkloadEnvironment, hc *unstructured.Unstructured, p HyperShiftProfile) (bool, error) {
	hcp := hyperShiftObject(hostedControlPlaneGVK, hc.GetNamespace()+"-"+hc.GetName(), hc.GetName())
	if err := r.hyperShiftReader().Get(ctx, client.ObjectKeyFromObject(hcp), hcp); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	infraID, _, _ := unstructured.NestedString(hc.Object, "spec", "infraID")
	hcpInfraID, _, _ := unstructured.NestedString(hcp.Object, "spec", "infraID")
	if infraID == "" || infraID != hcpInfraID || hcp.GetAnnotations()["hypershift.openshift.io/cluster"] != hc.GetNamespace()+"/"+hc.GetName() || hcp.GetUID() == "" || hcp.GetDeletionTimestamp() != nil {
		return false, fmt.Errorf("HostedControlPlane identity does not match the recorded HostedCluster")
	}
	route := hyperShiftObject(openShiftRouteGVK, hcp.GetNamespace(), "lwe-api")
	route.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: hostedControlPlaneGVK.GroupVersion().String(), Kind: hostedControlPlaneGVK.Kind, Name: hcp.GetName(), UID: hcp.GetUID()}})
	route.Object["spec"] = map[string]interface{}{
		"host":           hyperShiftAPIHost(e, p),
		"to":             map[string]interface{}{"kind": "Service", "name": "kube-apiserver", "weight": int64(100)},
		"port":           map[string]interface{}{"targetPort": int64(6443)},
		"tls":            map[string]interface{}{"termination": "passthrough", "insecureEdgeTerminationPolicy": "None"},
		"wildcardPolicy": "None",
	}
	actual, err := r.ensureHyperShiftManagedObject(ctx, e, route)
	if err != nil {
		return false, err
	}
	if _, labeled := actual.GetLabels()["hypershift.openshift.io/hosted-control-plane"]; labeled {
		return false, fmt.Errorf("shared API Route must not select HyperShift's dedicated router")
	}
	owned := false
	for _, owner := range actual.GetOwnerReferences() {
		if owner.UID == hcp.GetUID() && owner.Kind == hcp.GetKind() && owner.Name == hcp.GetName() && owner.APIVersion == hcp.GetAPIVersion() {
			owned = true
		}
	}
	if !owned {
		return false, fmt.Errorf("shared API Route is not owned by the current HostedControlPlane")
	}
	ingresses, _, _ := unstructured.NestedSlice(actual.Object, "status", "ingress")
	for _, item := range ingresses {
		ingress, ok := item.(map[string]interface{})
		if !ok || ingress["host"] != hyperShiftAPIHost(e, p) {
			continue
		}
		conditions, _ := ingress["conditions"].([]interface{})
		for _, item := range conditions {
			condition, ok := item.(map[string]interface{})
			if ok && condition["type"] == "Admitted" && condition["status"] == "True" {
				return true, nil
			}
		}
	}
	return false, nil
}
