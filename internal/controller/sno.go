package controller

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var snoGVK = schema.GroupVersionKind{Group: "sno.klape.io", Version: "v1alpha1", Kind: "SNOVirtualMachine"}

const snoRequestName = "sno"

type snoObservation struct {
	Kubeconfig []byte
	Ready      bool
	Reason     string
	Message    string
}

func newSNO(namespace string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(snoGVK)
	object.SetName(snoRequestName)
	object.SetNamespace(namespace)
	return object
}

func snoImageProfile(environment *environmentv1alpha1.LLMWorkloadEnvironment) string {
	if environment.Spec.SNO != nil && environment.Spec.SNO.ImageProfile != "" {
		return environment.Spec.SNO.ImageProfile
	}
	return "default"
}

func (r *LLMWorkloadEnvironmentReconciler) ensureSNO(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment, namespace string) (snoObservation, error) {
	waiting := snoObservation{Reason: "SNOProvisioning", Message: "SNO provider is preparing an independent cluster and external API"}
	if !r.isOpenShift() {
		return snoObservation{Reason: "SNOPlatformNotConfigured", Message: "SNO mode requires OpenShift Virtualization"}, nil
	}
	request := newSNO(namespace)
	err := r.Get(ctx, client.ObjectKeyFromObject(request), request)
	if apimeta.IsNoMatchError(err) {
		return snoObservation{Reason: "SNOProviderNotAvailable", Message: "Install the SNO VM operator and its SNOVirtualMachine API"}, nil
	}
	if apierrors.IsNotFound(err) {
		if ref := environment.Status.SNOVirtualMachineRef; ref != nil && ref.UID != "" {
			return snoObservation{Reason: "SNORequestMissing", Message: "Established SNO request is missing; recreate the environment explicitly"}, nil
		}
		request.Object["spec"] = map[string]interface{}{
			"imageProfile":            snoImageProfile(environment),
			"rootDiskRetentionPolicy": "Delete",
			"resources":               map[string]interface{}{"cores": int64(16), "memory": "128Gi", "rootDisk": "500Gi"},
		}
		// The environment and SNO request live in different namespaces. Use the
		// established LWE ownership annotations, never a cross-namespace ownerRef.
		markEnvironmentOwned(request, environment)
		if err = r.Create(ctx, request); err != nil {
			if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				return snoObservation{Reason: "SNOProviderNotAvailable", Message: "SNO provider API is not available"}, nil
			}
			return waiting, err
		}
	} else if err != nil {
		return waiting, err
	}
	if !environmentOwns(request, environment) {
		return waiting, fmt.Errorf("SNO request collision in backing namespace")
	}
	if ref := environment.Status.SNOVirtualMachineRef; ref != nil && ref.UID != "" && ref.UID != request.GetUID() {
		return waiting, fmt.Errorf("established SNO request identity has changed")
	}
	profile, _, _ := unstructured.NestedString(request.Object, "spec", "imageProfile")
	retention, _, _ := unstructured.NestedString(request.Object, "spec", "rootDiskRetentionPolicy")
	if profile != snoImageProfile(environment) || retention != "Delete" {
		return waiting, fmt.Errorf("existing SNO request does not match the immutable environment contract")
	}
	reference := &corev1.ObjectReference{APIVersion: snoGVK.GroupVersion().String(), Kind: snoGVK.Kind, Namespace: namespace, Name: request.GetName(), UID: request.GetUID()}
	if !reflect.DeepEqual(environment.Status.SNOVirtualMachineRef, reference) {
		environment.Status.SNOVirtualMachineRef = reference
		if err = r.Status().Update(ctx, environment); err != nil {
			return waiting, err
		}
	}
	if request.GetDeletionTimestamp() != nil {
		return snoObservation{Reason: "SNODeleting", Message: "SNO request is deleting"}, nil
	}
	conditions, _, _ := unstructured.NestedSlice(request.Object, "status", "conditions")
	ready := false
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok || condition["type"] != "Ready" {
			continue
		}
		generation, _, _ := unstructured.NestedInt64(condition, "observedGeneration")
		if generation != request.GetGeneration() {
			continue
		}
		ready = condition["status"] == string(metav1.ConditionTrue)
		if message, ok := condition["message"].(string); ok && message != "" {
			waiting.Message = message
		}
	}
	if !ready {
		return waiting, nil
	}
	secretName, _, _ := unstructured.NestedString(request.Object, "status", "credentialsSecretRef", "name")
	endpoint, _, _ := unstructured.NestedString(request.Object, "status", "apiEndpoint")
	if secretName == "" || endpoint == "" {
		return snoObservation{Reason: "SNOCredentialsNotReady", Message: "SNO provider has not published its external API credentials"}, nil
	}
	secret := &corev1.Secret{}
	if err = r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return snoObservation{Reason: "SNOCredentialsNotReady", Message: "Waiting for SNO access Secret"}, nil
		}
		return waiting, err
	}
	owned := false
	for _, owner := range secret.OwnerReferences {
		if owner.APIVersion == snoGVK.GroupVersion().String() && owner.Kind == snoGVK.Kind && owner.Name == request.GetName() && owner.UID == request.GetUID() {
			owned = true
		}
	}
	if !owned || request.GetUID() == "" {
		return waiting, fmt.Errorf("SNO access Secret is not owned by the provider request")
	}
	kubeconfig := secret.Data["kubeconfig"]
	if err = validateSNOAccess(kubeconfig, endpoint); err != nil {
		return waiting, err
	}
	return snoObservation{Ready: true, Kubeconfig: append([]byte(nil), kubeconfig...)}, nil
}

func validateSNOAccess(data []byte, endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("invalid SNO external API endpoint")
	}
	config, err := clientcmd.Load(data)
	if err != nil {
		return fmt.Errorf("invalid SNO access kubeconfig")
	}
	context := config.Contexts[config.CurrentContext]
	if context == nil {
		return fmt.Errorf("SNO kubeconfig has no current context")
	}
	cluster := config.Clusters[context.Cluster]
	if cluster == nil || strings.TrimSuffix(cluster.Server, "/") != strings.TrimSuffix(endpoint, "/") || cluster.InsecureSkipTLSVerify || len(cluster.CertificateAuthorityData) == 0 || cluster.CertificateAuthority != "" || cluster.ProxyURL != "" || (cluster.TLSServerName != "" && cluster.TLSServerName != u.Hostname()) {
		return fmt.Errorf("SNO kubeconfig must use the published endpoint and embedded guest CA with TLS verification")
	}
	auth := config.AuthInfos[context.AuthInfo]
	if auth == nil || auth.Exec != nil || auth.AuthProvider != nil || auth.TokenFile != "" || auth.ClientCertificate != "" || auth.ClientKey != "" || (auth.Token == "" && (len(auth.ClientCertificateData) == 0 || len(auth.ClientKeyData) == 0)) {
		return fmt.Errorf("SNO kubeconfig must contain portable inline credentials")
	}
	return nil
}

// finalizeSNO must complete before LWE deletes the backing namespace. Otherwise
// namespace GC could remove the SNO request before its storage finalizer runs.
func (r *LLMWorkloadEnvironmentReconciler) finalizeSNO(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment, namespace string) (bool, error) {
	request := newSNO(namespace)
	err := r.Get(ctx, client.ObjectKeyFromObject(request), request)
	if apimeta.IsNoMatchError(err) {
		if environment.Status.SNOVirtualMachineRef != nil {
			return false, fmt.Errorf("SNO provider API unavailable during cleanup")
		}
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !environmentOwns(request, environment) {
		return false, fmt.Errorf("refusing to remove a SNO request not owned by this environment")
	}
	if ref := environment.Status.SNOVirtualMachineRef; ref != nil && ref.UID != "" && ref.UID != request.GetUID() {
		return false, fmt.Errorf("refusing to remove a replacement SNO request")
	}
	retention, _, _ := unstructured.NestedString(request.Object, "spec", "rootDiskRetentionPolicy")
	if retention != "Delete" {
		return false, fmt.Errorf("SNO disk retention requires administrator review before namespace cleanup")
	}
	if request.GetDeletionTimestamp() == nil {
		uid := request.GetUID()
		if err = r.Delete(ctx, request, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}
