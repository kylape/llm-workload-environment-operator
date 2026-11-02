package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	envapi "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const hyperShiftImageDigestMirrorSetResourceName = "image-digest-mirrors"

type hyperShiftImageDigestMirrorSetResource struct {
	APIVersion string                          `json:"apiVersion"`
	Kind       string                          `json:"kind"`
	Metadata   hyperShiftImageDigestMirrorMeta `json:"metadata"`
	Spec       envapi.ImageDigestMirrorSetSpec `json:"spec"`
}

type hyperShiftImageDigestMirrorMeta struct {
	Name string `json:"name"`
}

func hyperShiftImageDigestMirrorSetConfig(e *envapi.LLMWorkloadEnvironment) (string, string, bool, error) {
	if e.Spec.HyperShift == nil || e.Spec.HyperShift.ImageDigestMirrorSet == nil {
		return "", "", false, nil
	}

	spec := *e.Spec.HyperShift.ImageDigestMirrorSet
	spec.ImageDigestMirrors = append([]envapi.ImageDigestMirror(nil), spec.ImageDigestMirrors...)
	for index := range spec.ImageDigestMirrors {
		if spec.ImageDigestMirrors[index].MirrorSourcePolicy == "" {
			spec.ImageDigestMirrors[index].MirrorSourcePolicy = envapi.NeverContactSource
		}
	}

	resource := hyperShiftImageDigestMirrorSetResource{
		APIVersion: "config.openshift.io/v1",
		Kind:       "ImageDigestMirrorSet",
		Metadata:   hyperShiftImageDigestMirrorMeta{Name: hyperShiftImageDigestMirrorSetResourceName},
		Spec:       spec,
	}
	serialized, err := json.Marshal(resource)
	if err != nil {
		return "", "", false, fmt.Errorf("serialize HyperShift ImageDigestMirrorSet: %w", err)
	}
	hash := sha256.Sum256(serialized)
	name := fmt.Sprintf("idms-%x", hash[:8])
	return name, string(serialized), true, nil
}

func hyperShiftImageDigestMirrorSetConfigName(e *envapi.LLMWorkloadEnvironment) string {
	name, _, configured, err := hyperShiftImageDigestMirrorSetConfig(e)
	if err != nil || !configured {
		return ""
	}
	return name
}

func (r *LLMWorkloadEnvironmentReconciler) ensureHyperShiftImageDigestMirrorSetConfig(
	ctx context.Context,
	e *envapi.LLMWorkloadEnvironment,
	namespace string,
) error {
	name, serialized, configured, err := hyperShiftImageDigestMirrorSetConfig(e)
	if err != nil || !configured {
		return err
	}

	wantData := map[string]string{"config": serialized}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	err = r.hyperShiftReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, config)
	if apierrors.IsNotFound(err) {
		config = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   namespace,
				Annotations: environmentOwnershipAnnotations(e),
			},
			Data:      wantData,
			Immutable: boolPointer(true),
		}
		return r.Create(ctx, config)
	}
	if err != nil {
		return fmt.Errorf("get HyperShift ImageDigestMirrorSet ConfigMap %s/%s: %w", namespace, name, err)
	}
	if !environmentOwns(config, e) {
		return fmt.Errorf("HyperShift ImageDigestMirrorSet ConfigMap %s/%s is owned by another environment", namespace, name)
	}
	if config.Immutable == nil || !*config.Immutable || len(config.BinaryData) != 0 || len(config.Data) != len(wantData) || config.Data["config"] != serialized {
		return fmt.Errorf("HyperShift ImageDigestMirrorSet ConfigMap %s/%s contents drifted", namespace, name)
	}
	return nil
}

func boolPointer(value bool) *bool {
	return &value
}
