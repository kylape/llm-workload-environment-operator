package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ApplicationProfileLabel = "llm.redhat.com/application-profile"
	ArgoCDSecretTypeLabel   = "argocd.argoproj.io/secret-type"
)

var (
	ErrArgoCDClusterSecretCollision = errors.New("Argo CD cluster Secret is not controlled by this environment")
	errArgoCDCleanupPending         = errors.New("Argo CD registration cleanup is pending")
)

// ValidateArgoCDNamespace accepts an empty namespace to disable registration.
func ValidateArgoCDNamespace(namespace string) error {
	if namespace != "" && len(validation.IsDNS1123Label(namespace)) != 0 {
		return errors.New("ARGOCD_NAMESPACE must be empty or a valid Kubernetes namespace name")
	}
	return nil
}

type argoCDClusterConfig struct {
	BearerToken        string                `json:"bearerToken,omitempty"`
	TLSClientConfig    argoCDTLSClientConfig `json:"tlsClientConfig"`
	DisableCompression bool                  `json:"disableCompression,omitempty"`
}

type argoCDTLSClientConfig struct {
	CAData     []byte `json:"caData,omitempty"`
	CertData   []byte `json:"certData,omitempty"`
	KeyData    []byte `json:"keyData,omitempty"`
	Insecure   bool   `json:"insecure"`
	ServerName string `json:"serverName,omitempty"`
}

// buildArgoCDClusterData reads embedded credentials only. In particular, it must
// not use a rest.Config loader that can read files or execute credential plugins.
func buildArgoCDClusterData(raw []byte, name string) (map[string][]byte, error) {
	config, err := clientcmd.Load(raw)
	if err != nil {
		// Decoder errors may include credential-bearing input. Never return them.
		return nil, errors.New("cannot decode environment kubeconfig")
	}
	selected := config.Contexts[config.CurrentContext]
	if config.CurrentContext == "" || selected == nil {
		return nil, errors.New("environment kubeconfig must select an existing current context")
	}
	cluster, user := config.Clusters[selected.Cluster], config.AuthInfos[selected.AuthInfo]
	if selected.Cluster == "" || selected.AuthInfo == "" || cluster == nil || user == nil {
		return nil, errors.New("environment kubeconfig context must reference an existing cluster and user")
	}
	server, err := url.Parse(cluster.Server)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" || server.User != nil || server.RawQuery != "" || server.Fragment != "" {
		return nil, errors.New("environment kubeconfig must use an HTTPS API endpoint without user info, query, or fragment")
	}
	if cluster.CertificateAuthority != "" || user.ClientCertificate != "" || user.ClientKey != "" || user.TokenFile != "" {
		return nil, errors.New("Argo CD registration requires embedded credentials and CA data; credential file paths are unsupported")
	}
	if user.Exec != nil || user.AuthProvider != nil || user.Username != "" || user.Password != "" ||
		user.Impersonate != "" || user.ImpersonateUID != "" || len(user.ImpersonateGroups) != 0 || len(user.ImpersonateUserExtra) != 0 || cluster.ProxyURL != "" {
		return nil, errors.New("environment kubeconfig uses unsupported authentication, impersonation, or proxy options")
	}
	hasCert, hasKey := len(user.ClientCertificateData) != 0, len(user.ClientKeyData) != 0
	if hasCert != hasKey || (user.Token == "" && !hasCert) {
		return nil, errors.New("environment kubeconfig requires a bearer token or a complete embedded client certificate and key")
	}
	connection := argoCDClusterConfig{
		BearerToken: user.Token,
		TLSClientConfig: argoCDTLSClientConfig{
			CAData: cluster.CertificateAuthorityData, CertData: user.ClientCertificateData, KeyData: user.ClientKeyData,
			Insecure: cluster.InsecureSkipTLSVerify, ServerName: cluster.TLSServerName,
		},
		DisableCompression: cluster.DisableCompression,
	}
	encoded, err := json.Marshal(connection)
	if err != nil {
		return nil, errors.New("cannot encode Argo CD connection configuration")
	}
	return map[string][]byte{"name": []byte(name), "server": []byte(cluster.Server), "config": encoded}, nil
}

func argoCDClusterSecretName(environment *environmentv1alpha1.LLMWorkloadEnvironment) string {
	return "lwe-" + string(environment.UID)
}

func argoCDRegistrationRequested(environment *environmentv1alpha1.LLMWorkloadEnvironment) bool {
	_, requested := environment.Labels[ApplicationProfileLabel]
	return requested
}

// Keep API errors useful without including admission responses that can echo
// credential-bearing Secret data. All registration paths use this boundary.
func argoCDAPIError(operation string, err error) error {
	return fmt.Errorf("%s: Kubernetes API %s", operation, apierrors.ReasonForError(err))
}

func (r *LLMWorkloadEnvironmentReconciler) setArgoCDSecretReference(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment, reference *corev1.SecretReference) error {
	before := environment.DeepCopy()
	environment.Status.ArgoCDClusterSecretRef = reference
	if err := r.patchEnvironmentStatus(ctx, environment, before); err != nil {
		return argoCDAPIError("record Argo CD registration reference", err)
	}
	// Read into a fresh object using an uncached client. Besides informer lag,
	// decoding a pruned status field back into an existing object can retain
	// its old in-memory value. Neither is proof that intent is durable.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	persisted := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(environment), persisted); err != nil {
		return argoCDAPIError("verify Argo CD registration reference", err)
	}
	if persisted.UID != environment.UID || !reflect.DeepEqual(persisted.Status.ArgoCDClusterSecretRef, reference) {
		return errors.New("Argo CD registration reference was not persisted; install the updated LWE CRD before enabling registration")
	}
	if reference != nil && (persisted.DeletionTimestamp != nil || !argoCDRegistrationRequested(persisted) ||
		persisted.Labels[ApplicationProfileLabel] != environment.Labels[ApplicationProfileLabel]) {
		return errors.New("environment registration request changed; retry with the current environment")
	}
	return nil
}

// prepareArgoCDRegistration runs before provisioning, so opt-out still works
// when a queue is no longer allowed or the environment cannot become ready.
func (r *LLMWorkloadEnvironmentReconciler) prepareArgoCDRegistration(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment) error {
	if err := ValidateArgoCDNamespace(r.ArgoCDNamespace); err != nil {
		return err
	}
	ref := environment.Status.ArgoCDClusterSecretRef
	if ref == nil {
		return nil
	}
	if argoCDRegistrationRequested(environment) && supportsClusterRegistration(environment) &&
		r.ArgoCDNamespace != "" && ref.Namespace == r.ArgoCDNamespace && ref.Name == argoCDClusterSecretName(environment) {
		return nil
	}
	return r.removeArgoCDClusterSecret(ctx, environment)
}

func (r *LLMWorkloadEnvironmentReconciler) ensureArgoCDClusterSecret(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment, raw []byte) error {
	if environment.UID == "" {
		return errors.New("environment UID is required for Argo CD registration")
	}
	name := argoCDClusterSecretName(environment)
	data, err := buildArgoCDClusterData(raw, name)
	if err != nil {
		return err
	}
	ref := &corev1.SecretReference{Namespace: r.ArgoCDNamespace, Name: name}
	if current := environment.Status.ArgoCDClusterSecretRef; current != nil && *current != *ref {
		return errors.New("previous Argo CD registration must be removed before creating a new one")
	}
	if err := r.setArgoCDSecretReference(ctx, environment, ref); err != nil {
		return err
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	err = r.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace}, Type: corev1.SecretTypeOpaque, Data: data}
		markEnvironmentOwned(secret, environment)
		secret.Labels = map[string]string{ArgoCDSecretTypeLabel: "cluster", ApplicationProfileLabel: environment.Labels[ApplicationProfileLabel]}
		if err := r.Create(ctx, secret); err != nil {
			return argoCDAPIError("create Argo CD cluster Secret", err)
		}
		return nil
	}
	if err != nil {
		return argoCDAPIError("read Argo CD cluster Secret", err)
	}
	if !environmentOwns(secret, environment) {
		return ErrArgoCDClusterSecretCollision
	}
	if secret.DeletionTimestamp != nil {
		return errArgoCDCleanupPending
	}
	before := secret.DeepCopy()
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = data
	if secret.Labels == nil {
		secret.Labels = make(map[string]string)
	}
	secret.Labels[ArgoCDSecretTypeLabel] = "cluster"
	secret.Labels[ApplicationProfileLabel] = environment.Labels[ApplicationProfileLabel]
	markEnvironmentOwned(secret, environment)
	if reflect.DeepEqual(before.Data, secret.Data) && reflect.DeepEqual(before.Labels, secret.Labels) &&
		reflect.DeepEqual(before.Annotations, secret.Annotations) && before.Type == secret.Type {
		return nil
	}
	// Update uses the version read above. A concurrently replaced or edited
	// object conflicts rather than being adopted by a blind merge patch.
	if err := r.Update(ctx, secret); err != nil {
		return argoCDAPIError("update Argo CD cluster Secret", err)
	}
	return nil
}

func (r *LLMWorkloadEnvironmentReconciler) removeArgoCDClusterSecret(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment) error {
	ref := environment.Status.ArgoCDClusterSecretRef
	if ref == nil {
		return nil
	}
	if ref.Name != argoCDClusterSecretName(environment) || ref.Namespace == "" {
		return ErrArgoCDClusterSecretCollision
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setArgoCDSecretReference(ctx, environment, nil)
		}
		return argoCDAPIError("read Argo CD registration for cleanup", err)
	}
	if !environmentOwns(secret, environment) {
		return ErrArgoCDClusterSecretCollision
	}
	if secret.DeletionTimestamp == nil {
		preconditions := client.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion}
		if err := r.Delete(ctx, secret, preconditions); err != nil && !apierrors.IsNotFound(err) {
			return argoCDAPIError("delete Argo CD registration", err)
		}
	}
	// Keep the durable reference until a later read confirms absence, including
	// when a finalizer delays deletion or the controller restarts here.
	return errArgoCDCleanupPending
}

func (r *LLMWorkloadEnvironmentReconciler) reconcileArgoCDRegistration(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment, preparationErr, provisionErr error) error {
	condition := metav1.Condition{Type: environmentv1alpha1.ArgoCDRegisteredCondition, Status: metav1.ConditionFalse, ObservedGeneration: environment.Generation}
	var registrationErr error
	switch {
	case preparationErr != nil:
		registrationErr = preparationErr
	case !argoCDRegistrationRequested(environment):
		condition.Reason, condition.Message = "NotRequested", "No application profile label is present"
	case !supportsClusterRegistration(environment):
		condition.Reason, condition.Message = "UnsupportedMode", "Argo CD registration supports VCluster and HyperShift environments"
	case r.ArgoCDNamespace == "":
		condition.Reason, condition.Message = "IntegrationDisabled", "ARGOCD_NAMESPACE is not configured"
	case provisionErr != nil || !apimeta.IsStatusConditionTrue(environment.Status.Conditions, environmentv1alpha1.ReadyCondition):
		condition.Reason, condition.Message = "WaitingForEnvironment", "Waiting for ready environment credentials"
		if environment.Status.ArgoCDClusterSecretRef != nil {
			condition.Status = metav1.ConditionUnknown
			condition.Reason, condition.Message = "EnvironmentUnavailable", "Environment credentials cannot currently be refreshed; the recorded registration is retained"
		}
	default:
		registrationErr = r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig)
		condition.Status = metav1.ConditionTrue
		condition.Reason, condition.Message = "Registered", "Argo CD cluster Secret is synchronized"
	}
	if registrationErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason, condition.Message = "RegistrationFailed", registrationErr.Error()
		if errors.Is(registrationErr, errArgoCDCleanupPending) {
			condition.Reason = "Unregistering"
		}
	}
	// ensureArgoCDClusterSecret may have persisted the reference. Use the
	// latest resource version and status as the base for this second patch.
	before := environment.DeepCopy()
	apimeta.SetStatusCondition(&environment.Status.Conditions, condition)
	if err := r.patchEnvironmentStatus(ctx, environment, before); err != nil {
		return errors.Join(registrationErr, argoCDAPIError("record Argo CD registration condition", err))
	}
	return registrationErr
}

func (r *LLMWorkloadEnvironmentReconciler) mapArgoCDSecret(ctx context.Context, object client.Object) []reconcile.Request {
	secret, ok := object.(*corev1.Secret)
	if !ok {
		return nil
	}
	owner, uid := secret.Annotations[EnvironmentOwnerAnnotation], secret.Annotations[EnvironmentUIDAnnotation]
	if strings.HasPrefix(secret.Name, "vc-") && secret.Namespace == NamespacePrefix+strings.TrimPrefix(secret.Name, "vc-") {
		namespace := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: secret.Namespace}, namespace); err != nil {
			return nil
		}
		owner, uid = namespace.Annotations[EnvironmentOwnerAnnotation], namespace.Annotations[EnvironmentUIDAnnotation]
	}
	parts := strings.Split(owner, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || uid == "" {
		return nil
	}
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{}
	key := types.NamespacedName{Namespace: parts[0], Name: parts[1]}
	if err := r.Get(ctx, key, environment); err != nil || string(environment.UID) != uid {
		return nil
	}
	ref := environment.Status.ArgoCDClusterSecretRef
	registration := secret.Name == argoCDClusterSecretName(environment) &&
		((ref != nil && ref.Namespace == secret.Namespace && ref.Name == secret.Name) || (r.ArgoCDNamespace != "" && secret.Namespace == r.ArgoCDNamespace))
	source := supportsClusterRegistration(environment) &&
		secret.Namespace == NamespacePrefix+environment.Name && secret.Name == "vc-"+environment.Name
	if registration || source {
		return []reconcile.Request{{NamespacedName: key}}
	}
	return nil
}

func argoCDSecretPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return relevantArgoCDSecret(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return relevantArgoCDSecret(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return relevantArgoCDSecret(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, oldOK := e.ObjectOld.(*corev1.Secret)
			current, currentOK := e.ObjectNew.(*corev1.Secret)
			return oldOK && currentOK && (relevantArgoCDSecret(old) || relevantArgoCDSecret(current)) &&
				(!reflect.DeepEqual(old.Data, current.Data) || old.Type != current.Type ||
					!reflect.DeepEqual(old.Labels, current.Labels) || !reflect.DeepEqual(old.Annotations, current.Annotations) ||
					!reflect.DeepEqual(old.DeletionTimestamp, current.DeletionTimestamp))
		},
	}
}

func relevantArgoCDSecret(object client.Object) bool {
	if object == nil {
		return false
	}
	return (strings.HasPrefix(object.GetName(), "lwe-") && object.GetAnnotations()[EnvironmentOwnerAnnotation] != "") ||
		(strings.HasPrefix(object.GetName(), "vc-") && object.GetNamespace() == NamespacePrefix+strings.TrimPrefix(object.GetName(), "vc-"))
}

func (r *LLMWorkloadEnvironmentReconciler) finalizeArgoCDRegistration(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment) error {
	err := r.removeArgoCDClusterSecret(ctx, environment)
	if errors.Is(err, ErrArgoCDClusterSecretCollision) {
		ctrl.LoggerFrom(ctx).Info("Leaving unrelated Argo CD Secret in place during finalization", "environment", client.ObjectKeyFromObject(environment))
		return r.setArgoCDSecretReference(ctx, environment, nil)
	}
	return err
}

func supportsClusterRegistration(environment *environmentv1alpha1.LLMWorkloadEnvironment) bool {
	mode := environmentMode(environment)
	return mode == environmentv1alpha1.EnvironmentModeVCluster || mode == environmentv1alpha1.EnvironmentModeHyperShift
}
