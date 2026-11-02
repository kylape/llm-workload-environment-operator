package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	hyperShiftWorkerStorageClassName = "worker-lvm"
	defaultStorageClassAnnotation    = "storageclass.kubernetes.io/is-default-class"
)

func ensureHyperShiftWorkerStorageClassDefault(ctx context.Context, kubeconfig []byte) (bool, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return false, fmt.Errorf("guest API client configuration is invalid: %w", err)
	}
	config.Timeout = 5 * time.Second
	guest, err := kubernetes.NewForConfig(config)
	if err != nil {
		return false, fmt.Errorf("create guest API client: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	storageClasses, err := guest.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list guest StorageClasses: %w", err)
	}

	workerStorageClassFound := false
	for _, storageClass := range storageClasses.Items {
		if storageClass.Name == hyperShiftWorkerStorageClassName {
			workerStorageClassFound = true
			break
		}
	}
	if !workerStorageClassFound {
		return false, nil
	}

	// Clear any competing default first, then set worker-lvm as the sole default.
	for _, storageClass := range storageClasses.Items {
		if storageClass.Name != hyperShiftWorkerStorageClassName && storageClass.Annotations[defaultStorageClassAnnotation] == "true" {
			if err := patchStorageClassDefault(ctx, guest, storageClass.Name, "false"); err != nil {
				return false, err
			}
		}
	}
	for _, storageClass := range storageClasses.Items {
		if storageClass.Name == hyperShiftWorkerStorageClassName && storageClass.Annotations[defaultStorageClassAnnotation] != "true" {
			if err := patchStorageClassDefault(ctx, guest, storageClass.Name, "true"); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func patchStorageClassDefault(ctx context.Context, guest kubernetes.Interface, name, value string) error {
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{defaultStorageClassAnnotation: value},
		},
	})
	if err != nil {
		return fmt.Errorf("encode default StorageClass patch: %w", err)
	}
	if _, err := guest.StorageV1().StorageClasses().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch default annotation on guest StorageClass %q: %w", name, err)
	}
	return nil
}
