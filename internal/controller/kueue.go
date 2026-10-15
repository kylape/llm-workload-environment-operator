package controller

import (
	"context"
	"errors"
	"fmt"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	kueueAPIGroup = "kueue.x-k8s.io"
	kueueV1Beta1  = "v1beta1"
	kueueV1Beta2  = "v1beta2"
)

var ErrKueueAPIUnavailable = errors.New("neither supported Kueue API version is available")

// KueueDiscovery is the portion of Kubernetes discovery used to select a
// served Kueue API version.
type KueueDiscovery interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// KueueClient hides the Kueue API version from reconciliation logic.
type KueueClient interface {
	EnsureClusterQueueExists(ctx context.Context, name string) error
	EnsureLocalQueue(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment, namespace, name string) (bool, error)
}

// KueueAdapter uses the newest compatible API served by the cluster.
type KueueAdapter struct {
	client    client.Client
	discovery KueueDiscovery
}

func NewKueueAdapter(kubeClient client.Client, discovery KueueDiscovery) *KueueAdapter {
	return &KueueAdapter{client: kubeClient, discovery: discovery}
}

func (a *KueueAdapter) groupVersion() (schema.GroupVersion, error) {
	if a.discovery == nil {
		return schema.GroupVersion{}, fmt.Errorf("%w: Kueue API discovery is not configured", ErrKueueAPIUnavailable)
	}

	for _, version := range []string{kueueV1Beta2, kueueV1Beta1} {
		groupVersion := schema.GroupVersion{Group: kueueAPIGroup, Version: version}
		resources, err := a.discovery.ServerResourcesForGroupVersion(groupVersion.String())
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return schema.GroupVersion{}, fmt.Errorf("discover Kueue API %s: %w", groupVersion, err)
		}
		if servesKueueQueueResources(resources) {
			return groupVersion, nil
		}
	}

	return schema.GroupVersion{}, fmt.Errorf(
		"%w: cluster must serve ClusterQueue and LocalQueue through %s or %s",
		ErrKueueAPIUnavailable,
		schema.GroupVersion{Group: kueueAPIGroup, Version: kueueV1Beta2},
		schema.GroupVersion{Group: kueueAPIGroup, Version: kueueV1Beta1},
	)
}

func servesKueueQueueResources(resources *metav1.APIResourceList) bool {
	if resources == nil {
		return false
	}
	clusterQueue, localQueue := false, false
	for _, resource := range resources.APIResources {
		switch resource.Name {
		case "clusterqueues":
			clusterQueue = true
		case "localqueues":
			localQueue = true
		}
	}
	return clusterQueue && localQueue
}

func (a *KueueAdapter) EnsureClusterQueueExists(ctx context.Context, name string) error {
	groupVersion, err := a.groupVersion()
	if err != nil {
		return err
	}

	queue := newKueueObject(groupVersion, "ClusterQueue", name, "")
	if err := a.client.Get(ctx, types.NamespacedName{Name: name}, queue); err != nil {
		return fmt.Errorf("get Kueue ClusterQueue %q through %s: %w", name, groupVersion, err)
	}
	return nil
}

func (a *KueueAdapter) EnsureLocalQueue(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace, name string,
) (bool, error) {
	groupVersion, err := a.groupVersion()
	if err != nil {
		return false, err
	}

	queue := newKueueObject(groupVersion, "LocalQueue", name, namespace)
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := a.client.Get(ctx, key, queue); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get Kueue LocalQueue %s/%s through %s: %w", namespace, name, groupVersion, err)
	} else if err == nil && !environmentOwns(queue, environment) {
		return false, fmt.Errorf("%w: %s/%s", ErrLocalQueueCollision, namespace, name)
	}

	_, err = controllerutil.CreateOrUpdate(ctx, a.client, queue, func() error {
		if err := unstructured.SetNestedField(queue.Object, requestedClusterQueue(environment), "spec", "clusterQueue"); err != nil {
			return err
		}
		markEnvironmentOwned(queue, environment)
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("ensure Kueue LocalQueue %s/%s through %s: %w", namespace, name, groupVersion, err)
	}
	return localQueueActive(queue), nil
}

func newKueueObject(groupVersion schema.GroupVersion, kind, name, namespace string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(groupVersion.WithKind(kind))
	object.SetName(name)
	object.SetNamespace(namespace)
	return object
}

func localQueueActive(queue *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(queue.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, rawCondition := range conditions {
		condition, ok := rawCondition.(map[string]interface{})
		if !ok {
			continue
		}
		conditionType, _ := condition["type"].(string)
		status, _ := condition["status"].(string)
		if conditionType == "Active" && status == string(metav1.ConditionTrue) {
			return true
		}
	}
	return false
}
