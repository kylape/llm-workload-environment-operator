package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"

	envapi "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const hyperShiftProfileAnnotation = "llm.redhat.com/hypershift-profile-hash"

var hostedClusterGVK = schema.GroupVersionKind{Group: "hypershift.openshift.io", Version: "v1beta1", Kind: "HostedCluster"}
var nodePoolGVK = schema.GroupVersionKind{Group: "hypershift.openshift.io", Version: "v1beta1", Kind: "NodePool"}

func (r *LLMWorkloadEnvironmentReconciler) hyperShiftReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func hyperShiftObject(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	o.SetNamespace(namespace)
	o.SetName(name)
	return o
}

func hyperShiftOptions(e *envapi.LLMWorkloadEnvironment) (string, int32) {
	profile, replicas := "default", int32(1)
	if e.Spec.HyperShift != nil {
		if e.Spec.HyperShift.Profile != "" {
			profile = e.Spec.HyperShift.Profile
		}
		if e.Spec.HyperShift.Replicas != nil {
			replicas = *e.Spec.HyperShift.Replicas
		}
	}
	return profile, replicas
}

// Optional provider resources are read without cache-backed watches, allowing
// Namespace and VCluster mode to run where HyperShift CRDs are not installed.
func (r *LLMWorkloadEnvironmentReconciler) ensureHyperShift(ctx context.Context, e *envapi.LLMWorkloadEnvironment, namespace string) ([]byte, bool, error) {
	if r.HyperShift == nil || !r.HyperShift.Enabled || !r.isOpenShift() {
		return nil, false, fmt.Errorf("HyperShift provisioning is disabled or the host is not OpenShift")
	}
	name, replicas := hyperShiftOptions(e)
	p, ok := r.HyperShift.Profiles[name]
	if !ok {
		return nil, false, fmt.Errorf("HyperShift profile %q is not configured", name)
	}
	p.defaults()
	if err := p.validate(); err != nil {
		return nil, false, err
	}
	if replicas < 1 || replicas > p.MaxReplicas {
		return nil, false, fmt.Errorf("worker replicas must be between 1 and profile maximum %d", p.MaxReplicas)
	}
	profileJSON, _ := json.Marshal(p)
	profileHash := fmt.Sprintf("%x", sha256.Sum256(profileJSON))
	hc := hyperShiftObject(hostedClusterGVK, namespace, "cluster")
	err := r.hyperShiftReader().Get(ctx, client.ObjectKeyFromObject(hc), hc)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	if apierrors.IsNotFound(err) && e.Status.HostedClusterRef != nil {
		return nil, false, fmt.Errorf("recorded HostedCluster is missing; create a new environment rather than replacing cluster identity")
	}
	if err == nil {
		if err := checkHyperShiftIdentity(hc, e, e.Status.HostedClusterRef); err != nil {
			return nil, false, err
		}
		if hc.GetAnnotations()[hyperShiftProfileAnnotation] != profileHash {
			return nil, false, fmt.Errorf("administrator profile changed; restore it or create an environment with a new profile")
		}
	}
	if err := r.ensureHyperShiftCredentials(ctx, e, namespace, p, apierrors.IsNotFound(err)); err != nil {
		return nil, false, err
	}
	desiredHC, desiredNP := hyperShiftResources(e, namespace, p, replicas)
	if apierrors.IsNotFound(err) {
		hc = desiredHC
		annotations := hc.GetAnnotations()
		annotations[hyperShiftProfileAnnotation] = profileHash
		hc.SetAnnotations(annotations)
		if err := r.Create(ctx, hc); err != nil {
			return nil, false, err
		}
	} else if !hyperShiftSubset(desiredHC.Object["spec"], hc.Object["spec"]) {
		return nil, false, fmt.Errorf("HostedCluster configuration drifted from its administrator profile")
	}
	if hc.GetDeletionTimestamp() != nil {
		return nil, false, fmt.Errorf("HostedCluster is being deleted")
	}
	if err := r.recordHyperShiftReference(ctx, e, hc); err != nil {
		return nil, false, err
	}
	np := hyperShiftObject(nodePoolGVK, namespace, "workers")
	err = r.hyperShiftReader().Get(ctx, client.ObjectKeyFromObject(np), np)
	if apierrors.IsNotFound(err) {
		if e.Status.NodePoolRef != nil {
			return nil, false, fmt.Errorf("recorded NodePool is missing")
		}
		np = desiredNP
		np.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: hostedClusterGVK.GroupVersion().String(), Kind: hostedClusterGVK.Kind, Name: hc.GetName(), UID: hc.GetUID()}})
		if err := r.Create(ctx, np); err != nil {
			return nil, false, err
		}
	} else if err != nil {
		return nil, false, err
	} else {
		if err := checkHyperShiftIdentity(np, e, e.Status.NodePoolRef); err != nil {
			return nil, false, err
		}
		actual := np.DeepCopy()
		unstructured.RemoveNestedField(actual.Object, "spec", "replicas")
		desired := desiredNP.DeepCopy()
		unstructured.RemoveNestedField(desired.Object, "spec", "replicas")
		if !hyperShiftSubset(desired.Object["spec"], actual.Object["spec"]) {
			return nil, false, fmt.Errorf("NodePool configuration drifted from its administrator profile")
		}
		current, _, _ := unstructured.NestedInt64(np.Object, "spec", "replicas")
		if current != int64(replicas) {
			before := np.DeepCopy()
			_ = unstructured.SetNestedField(np.Object, int64(replicas), "spec", "replicas")
			if err := r.Patch(ctx, np, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, false, err
			}
			return nil, false, nil
		}
	}
	if err := r.recordHyperShiftReference(ctx, e, np); err != nil {
		return nil, false, err
	}
	if np.GetDeletionTimestamp() != nil || !hyperShiftCondition(hc, "Available") || !hyperShiftCondition(hc, "ClusterVersionAvailable") || !hyperShiftCondition(np, "Ready") || !hyperShiftCondition(np, "AllNodesHealthy") {
		return nil, false, nil
	}
	actualReplicas, _, _ := unstructured.NestedInt64(np.Object, "status", "replicas")
	if actualReplicas != int64(replicas) {
		return nil, false, nil
	}
	config, err := r.hyperShiftKubeconfig(ctx, hc)
	if err != nil {
		return nil, false, err
	}
	if err := probeHyperShiftAPI(ctx, config, replicas); err != nil {
		return nil, false, nil
	}
	return config, true, nil
}

func (r *LLMWorkloadEnvironmentReconciler) recordHyperShiftReference(ctx context.Context, e *envapi.LLMWorkloadEnvironment, o *unstructured.Unstructured) error {
	before := e.DeepCopy()
	ref := &corev1.ObjectReference{APIVersion: o.GetAPIVersion(), Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), UID: o.GetUID()}
	if o.GetKind() == hostedClusterGVK.Kind {
		e.Status.HostedClusterRef = ref
	} else {
		e.Status.NodePoolRef = ref
	}
	return r.patchEnvironmentStatus(ctx, e, before)
}

func checkHyperShiftIdentity(o *unstructured.Unstructured, e *envapi.LLMWorkloadEnvironment, ref *corev1.ObjectReference) error {
	if !environmentOwns(o, e) {
		return fmt.Errorf("%s %s is owned by another environment", o.GetKind(), o.GetName())
	}
	if ref != nil && (ref.UID != o.GetUID() || ref.Name != o.GetName() || ref.Namespace != o.GetNamespace()) {
		return fmt.Errorf("%s identity was replaced", o.GetKind())
	}
	return nil
}

func hyperShiftCondition(o *unstructured.Unstructured, name string) bool {
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == name {
			return m["status"] == "True" && m["observedGeneration"] == o.GetGeneration()
		}
	}
	return false
}

// Compare only managed fields; admission defaults, generated IDs and allocated
// API NodePorts belong to HyperShift and must not be overwritten.
func hyperShiftSubset(want, actual interface{}) bool {
	switch w := want.(type) {
	case map[string]interface{}:
		if len(w) == 0 && actual == nil {
			return true
		}
		a, ok := actual.(map[string]interface{})
		if !ok {
			return false
		}
		for k, v := range w {
			if !hyperShiftSubset(v, a[k]) {
				return false
			}
		}
		return true
	case []interface{}:
		a, ok := actual.([]interface{})
		if !ok || len(a) != len(w) {
			return false
		}
		for i := range w {
			if !hyperShiftSubset(w[i], a[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, actual)
	}
}

func hyperShiftResources(e *envapi.LLMWorkloadEnvironment, namespace string, p HyperShiftProfile, replicas int32) (*unstructured.Unstructured, *unstructured.Unstructured) {
	id := sha256.Sum256([]byte(e.UID))
	hc := hyperShiftObject(hostedClusterGVK, namespace, "cluster")
	hc.SetAnnotations(environmentOwnershipAnnotations(e))
	hc.Object["spec"] = map[string]interface{}{
		"release":                      map[string]interface{}{"image": p.ReleaseImage},
		"pullSecret":                   map[string]interface{}{"name": "hypershift-pull-secret"},
		"infraID":                      fmt.Sprintf("lwe-%x", id[:6]),
		"controllerAvailabilityPolicy": "SingleReplica", "infrastructureAvailabilityPolicy": "SingleReplica",
		"dns":              map[string]interface{}{"baseDomain": p.BaseDomain, "baseDomainPrefix": fmt.Sprintf("%s-%x", e.Name, id[:5])},
		"nodeSelector":     stringMap(p.ControlPlaneNodeSelector),
		"networking":       map[string]interface{}{"networkType": "OVNKubernetes", "clusterNetwork": []interface{}{map[string]interface{}{"cidr": p.ClusterNetwork}}, "serviceNetwork": []interface{}{map[string]interface{}{"cidr": p.ServiceNetwork}}},
		"etcd":             map[string]interface{}{"managementType": "Managed", "managed": map[string]interface{}{"storage": map[string]interface{}{"type": "PersistentVolume", "persistentVolume": map[string]interface{}{"size": p.EtcdStorageSize, "storageClassName": p.EtcdStorageClass}}}},
		"secretEncryption": map[string]interface{}{"type": "aescbc", "aescbc": map[string]interface{}{"activeKey": map[string]interface{}{"name": "hypershift-etcd-key"}}},
		"platform":         map[string]interface{}{"type": "KubeVirt", "kubevirt": map[string]interface{}{"baseDomainPassthrough": true, "storageDriver": map[string]interface{}{"type": "Manual", "manual": map[string]interface{}{"storageClassMapping": []interface{}{map[string]interface{}{"guestStorageClassName": "worker-lvm", "infraStorageClassName": p.WorkerStorageClass}}}}}},
		"services": []interface{}{
			map[string]interface{}{"service": "APIServer", "servicePublishingStrategy": map[string]interface{}{"type": "NodePort", "nodePort": map[string]interface{}{"address": p.APIServerAddress}}},
			map[string]interface{}{"service": "Ignition", "servicePublishingStrategy": map[string]interface{}{"type": "Route"}},
			map[string]interface{}{"service": "Konnectivity", "servicePublishingStrategy": map[string]interface{}{"type": "Route"}},
			map[string]interface{}{"service": "OAuthServer", "servicePublishingStrategy": map[string]interface{}{"type": "Route"}},
		},
		"olmCatalogPlacement": "management",
	}
	if p.SSHKey != nil {
		_ = unstructured.SetNestedMap(hc.Object, map[string]interface{}{"name": "hypershift-ssh-key"}, "spec", "sshKey")
	}
	np := hyperShiftObject(nodePoolGVK, namespace, "workers")
	np.SetAnnotations(environmentOwnershipAnnotations(e))
	np.Object["spec"] = map[string]interface{}{
		"clusterName": "cluster", "replicas": int64(replicas), "arch": "amd64",
		"release":    map[string]interface{}{"image": p.ReleaseImage},
		"management": map[string]interface{}{"autoRepair": false, "upgradeType": "Replace"},
		"platform": map[string]interface{}{"type": "KubeVirt", "kubevirt": map[string]interface{}{
			"attachDefaultNetwork": true, "compute": map[string]interface{}{"cores": int64(p.WorkerCores), "memory": p.WorkerMemory},
			"networkInterfaceMultiqueue": "Enable", "nodeSelector": stringMap(p.WorkerNodeSelector),
			"rootVolume": map[string]interface{}{"type": "Persistent", "cacheStrategy": map[string]interface{}{"type": "None"}, "persistent": map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}, "size": p.WorkerRootDisk, "storageClass": p.WorkerStorageClass, "volumeMode": "Block"}},
		}},
	}
	return hc, np
}

func stringMap(values map[string]string) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range values {
		out[k] = v
	}
	return out
}

func (r *LLMWorkloadEnvironmentReconciler) ensureHyperShiftCredentials(ctx context.Context, e *envapi.LLMWorkloadEnvironment, ns string, p HyperShiftProfile, creating bool) error {
	source := &corev1.Secret{}
	if err := r.hyperShiftReader().Get(ctx, types.NamespacedName{Namespace: p.PullSecret.Namespace, Name: p.PullSecret.Name}, source); err != nil {
		return err
	}
	var auth struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if json.Unmarshal(source.Data[corev1.DockerConfigJsonKey], &auth) != nil || len(auth.Auths) == 0 {
		return fmt.Errorf("profile pull Secret must contain a nonempty .dockerconfigjson auths map")
	}
	if err := r.ensureHyperShiftSecret(ctx, e, ns, "hypershift-pull-secret", corev1.SecretTypeDockerConfigJson, map[string][]byte{corev1.DockerConfigJsonKey: source.Data[corev1.DockerConfigJsonKey]}, false); err != nil {
		return err
	}
	key := &corev1.Secret{}
	err := r.hyperShiftReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: "hypershift-etcd-key"}, key)
	if apierrors.IsNotFound(err) && creating {
		bytes := make([]byte, 32)
		if _, err := rand.Read(bytes); err != nil {
			return err
		}
		if err := r.ensureHyperShiftSecret(ctx, e, ns, "hypershift-etcd-key", corev1.SecretTypeOpaque, map[string][]byte{"key": bytes}, true); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("etcd encryption key is unavailable: %w", err)
	} else if !environmentOwns(key, e) || len(key.Data["key"]) != 32 {
		return fmt.Errorf("etcd encryption Secret ownership or key length is invalid")
	}
	if p.SSHKey != nil {
		ssh := &corev1.Secret{}
		if err := r.hyperShiftReader().Get(ctx, types.NamespacedName{Namespace: p.SSHKey.Namespace, Name: p.SSHKey.Name}, ssh); err != nil {
			return err
		}
		if len(ssh.Data["id_rsa.pub"]) == 0 {
			return fmt.Errorf("SSH Secret is missing id_rsa.pub")
		}
		return r.ensureHyperShiftSecret(ctx, e, ns, "hypershift-ssh-key", corev1.SecretTypeOpaque, map[string][]byte{"id_rsa.pub": ssh.Data["id_rsa.pub"]}, false)
	}
	return nil
}

func (r *LLMWorkloadEnvironmentReconciler) ensureHyperShiftSecret(ctx context.Context, e *envapi.LLMWorkloadEnvironment, ns, name string, kind corev1.SecretType, data map[string][]byte, immutable bool) error {
	o := &corev1.Secret{}
	err := r.hyperShiftReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, o)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: environmentOwnershipAnnotations(e)}, Type: kind, Data: data, Immutable: &immutable})
	}
	if err != nil {
		return err
	}
	if !environmentOwns(o, e) {
		return fmt.Errorf("Secret %s belongs to another environment", name)
	}
	if o.Type == kind && reflect.DeepEqual(o.Data, data) {
		return nil
	}
	if immutable {
		return fmt.Errorf("refusing to replace immutable encryption material")
	}
	before := o.DeepCopy()
	o.Type, o.Data = kind, data
	return r.Patch(ctx, o, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *LLMWorkloadEnvironmentReconciler) finalizeHyperShift(ctx context.Context, e *envapi.LLMWorkloadEnvironment, ns string) (bool, error) {
	for _, resource := range []struct {
		gvk  schema.GroupVersionKind
		name string
		ref  *corev1.ObjectReference
	}{{nodePoolGVK, "workers", e.Status.NodePoolRef}, {hostedClusterGVK, "cluster", e.Status.HostedClusterRef}} {
		o := hyperShiftObject(resource.gvk, ns, resource.name)
		err := r.hyperShiftReader().Get(ctx, client.ObjectKeyFromObject(o), o)
		if apierrors.IsNotFound(err) {
			continue
		}
		if apimeta.IsNoMatchError(err) && resource.ref == nil {
			continue
		}
		if err != nil {
			return false, err
		}
		if err := checkHyperShiftIdentity(o, e, resource.ref); err != nil {
			return false, err
		}
		if o.GetDeletionTimestamp() == nil {
			uid := o.GetUID()
			if err := r.Delete(ctx, o, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); client.IgnoreNotFound(err) != nil {
				return false, err
			}
		}
		return false, nil
	}
	if e.Status.HostedClusterRef != nil {
		cp := &corev1.Namespace{}
		err := r.hyperShiftReader().Get(ctx, types.NamespacedName{Name: ns + "-cluster"}, cp)
		if err == nil {
			return false, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}
