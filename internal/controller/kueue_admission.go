package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metavalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

const kueueNamespaceLabelsAnnotation = "llm.redhat.com/kueue-namespace-labels"

// KueueAdmissionConfig is installation policy, never tenant CR configuration.
type KueueAdmissionConfig struct {
	Enabled           bool
	ValidationEnabled bool
	NamespaceLabels   map[string]string
	ConfigNamespace   string
	ConfigName        string
	ConfigKey         string
}

func DefaultKueueAdmissionConfig() KueueAdmissionConfig {
	return KueueAdmissionConfig{
		Enabled: true, ValidationEnabled: true,
		NamespaceLabels: map[string]string{KueueManagedLabel: "true"},
		ConfigNamespace: "kueue-system", ConfigName: "kueue-manager-config",
		ConfigKey: "controller_manager_config.yaml",
	}
}

// KueueAdmissionConfigFromEnv rejects malformed policy before starting the manager.
func KueueAdmissionConfigFromEnv(getenv func(string) string) (KueueAdmissionConfig, error) {
	c := DefaultKueueAdmissionConfig()
	if value := getenv("KUEUE_ADMISSION_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return c, fmt.Errorf("KUEUE_ADMISSION_ENABLED must be a boolean: %w", err)
		}
		c.Enabled = enabled
	}
	if value := getenv("KUEUE_ADMISSION_VALIDATION_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return c, fmt.Errorf("KUEUE_ADMISSION_VALIDATION_ENABLED must be a boolean: %w", err)
		}
		c.ValidationEnabled = enabled
	}
	if value := getenv("KUEUE_NAMESPACE_LABELS"); value != "" {
		// Decode into a fresh map: an override replaces the default label.
		var configured map[string]interface{}
		if err := json.Unmarshal([]byte(value), &configured); err != nil || configured == nil {
			return c, fmt.Errorf("KUEUE_NAMESPACE_LABELS must be a JSON object of string label values")
		}
		c.NamespaceLabels = make(map[string]string, len(configured))
		for key, value := range configured {
			text, ok := value.(string)
			if !ok {
				return c, fmt.Errorf("KUEUE_NAMESPACE_LABELS value for %q must be a string", key)
			}
			c.NamespaceLabels[key] = text
		}
	}
	for key, value := range c.NamespaceLabels {
		if errs := validation.IsQualifiedName(key); len(errs) != 0 {
			return c, fmt.Errorf("invalid KUEUE_NAMESPACE_LABELS key %q: %s", key, strings.Join(errs, ", "))
		}
		if errs := validation.IsValidLabelValue(value); len(errs) != 0 {
			return c, fmt.Errorf("invalid KUEUE_NAMESPACE_LABELS value for %q: %s", key, strings.Join(errs, ", "))
		}
		switch key {
		case "kubernetes.io/metadata.name", "app.kubernetes.io/managed-by", "llm.redhat.com/environment":
			return c, fmt.Errorf("KUEUE_NAMESPACE_LABELS cannot override reserved label %q", key)
		}
	}
	if value := getenv("KUEUE_CONFIG_REF"); value != "" {
		parts := strings.Split(value, ":")
		if len(parts) != 3 {
			return c, fmt.Errorf("KUEUE_CONFIG_REF must have the form namespace:name:key")
		}
		c.ConfigNamespace, c.ConfigName, c.ConfigKey = parts[0], parts[1], parts[2]
	}
	if errs := validation.IsDNS1123Label(c.ConfigNamespace); len(errs) != 0 {
		return c, fmt.Errorf("invalid KUEUE_CONFIG_REF namespace: %s", strings.Join(errs, ", "))
	}
	if errs := validation.IsDNS1123Subdomain(c.ConfigName); len(errs) != 0 {
		return c, fmt.Errorf("invalid KUEUE_CONFIG_REF name: %s", strings.Join(errs, ", "))
	}
	if errs := validation.IsConfigMapKey(c.ConfigKey); len(errs) != 0 {
		return c, fmt.Errorf("invalid KUEUE_CONFIG_REF key: %s", strings.Join(errs, ", "))
	}
	return c, nil
}

func (r *LLMWorkloadEnvironmentReconciler) kueueAdmissionConfig() KueueAdmissionConfig {
	if r.KueueAdmission == nil {
		return DefaultKueueAdmissionConfig()
	}
	return *r.KueueAdmission
}

// applyKueueNamespaceLabels removes only labels last written by this operator,
// preserving unrelated labels and administrator edits to retired keys.
func (r *LLMWorkloadEnvironmentReconciler) applyKueueNamespaceLabels(namespace *corev1.Namespace) error {
	config := r.kueueAdmissionConfig()
	if !config.Enabled {
		// Opt-out leaves existing policy metadata untouched, including legacy
		// records. Do not inspect or migrate it while Kueue is disabled.
		return nil
	}
	previous := map[string]string{}
	if recorded := namespace.Annotations[kueueNamespaceLabelsAnnotation]; recorded != "" {
		if err := json.Unmarshal([]byte(recorded), &previous); err != nil {
			return fmt.Errorf("invalid recorded Kueue namespace labels: %w", err)
		}
	} else {
		// Migrate the label used by operator versions predating configurable labels.
		previous[KueueManagedLabel] = "true"
	}
	desired := config.NamespaceLabels
	if namespace.Labels == nil {
		namespace.Labels = map[string]string{}
	}
	for key, value := range previous {
		switch key {
		case "kubernetes.io/metadata.name", "app.kubernetes.io/managed-by", "llm.redhat.com/environment":
			return fmt.Errorf("recorded Kueue namespace labels contain reserved label %q", key)
		}
		if _, keep := desired[key]; !keep && namespace.Labels[key] == value {
			delete(namespace.Labels, key)
		}
	}
	for key, value := range desired {
		namespace.Labels[key] = value
	}
	if namespace.Annotations == nil {
		namespace.Annotations = map[string]string{}
	}
	recorded, err := json.Marshal(desired)
	if err != nil {
		return err
	}
	namespace.Annotations[kueueNamespaceLabelsAnnotation] = string(recorded)
	return nil
}

// Only decode the policy fields we consume; Kueue has other versioned settings.
type kueueAdmissionConfiguration struct {
	metav1.TypeMeta              `json:",inline"`
	ManagedJobsNamespaceSelector *metav1.LabelSelector `json:"managedJobsNamespaceSelector"`
	Integrations                 struct {
		Frameworks []string `json:"frameworks"`
		PodOptions *struct {
			NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector"`
		} `json:"podOptions"`
	} `json:"integrations"`
}

func kueuePodNamespaceSelector(data []byte) (labels.Selector, error) {
	var config kueueAdmissionConfiguration
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("decode Kueue configuration: %w", err)
	}
	if config.Kind != "Configuration" || (config.APIVersion != "config.kueue.x-k8s.io/v1beta1" && config.APIVersion != "config.kueue.x-k8s.io/v1beta2") {
		return nil, fmt.Errorf("expected a config.kueue.x-k8s.io/v1beta1 or v1beta2 Configuration")
	}
	podEnabled := false
	for _, framework := range config.Integrations.Frameworks {
		podEnabled = podEnabled || framework == "pod"
	}
	if !podEnabled {
		return nil, fmt.Errorf("Kueue pod integration is not enabled")
	}
	selector := config.ManagedJobsNamespaceSelector
	if selector == nil && config.APIVersion == "config.kueue.x-k8s.io/v1beta1" && config.Integrations.PodOptions != nil {
		selector = config.Integrations.PodOptions.NamespaceSelector
	}
	if selector == nil {
		return nil, fmt.Errorf("Kueue namespace selection is implicit; configure managedJobsNamespaceSelector explicitly so LWE can validate it")
	}
	if errs := metavalidation.ValidateLabelSelector(selector, metavalidation.LabelSelectorValidationOptions{}, field.NewPath("namespaceSelector")); len(errs) != 0 {
		return nil, fmt.Errorf("invalid Kueue namespace selector: %w", errs.ToAggregate())
	}
	result, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("invalid Kueue namespace selector: %w", err)
	}
	return result, nil
}

func (r *LLMWorkloadEnvironmentReconciler) validateKueueAdmission(ctx context.Context, namespaceName string) (string, error) {
	config := r.kueueAdmissionConfig()
	if !config.Enabled || !config.ValidationEnabled {
		return "", nil
	}
	// Read live state, including namespace labels just written by reconciliation.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: config.ConfigNamespace, Name: config.ConfigName}, cm); err != nil {
		return "KueueAdmissionValidationFailed", fmt.Errorf("read Kueue ConfigMap %s/%s: %w", config.ConfigNamespace, config.ConfigName, err)
	}
	selector, err := kueuePodNamespaceSelector([]byte(cm.Data[config.ConfigKey]))
	if err != nil {
		return "KueueAdmissionConfigurationInvalid", fmt.Errorf("Kueue ConfigMap %s/%s key %q: %w", config.ConfigNamespace, config.ConfigName, config.ConfigKey, err)
	}
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: namespaceName}, namespace); err != nil {
		return "KueueAdmissionValidationFailed", fmt.Errorf("read backing namespace: %w", err)
	}
	if !selector.Matches(labels.Set(namespace.Labels)) {
		return "KueueNamespaceNotSelected", fmt.Errorf("backing namespace %q does not match Kueue namespace selector %q; configure KUEUE_NAMESPACE_LABELS or explicitly disable validation with KUEUE_ADMISSION_VALIDATION_ENABLED=false", namespaceName, selector.String())
	}
	return "", nil
}
