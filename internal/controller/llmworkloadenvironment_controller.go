package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	environmentvcluster "github.com/kylape/llm-workload-environment-operator/internal/vcluster"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	FinalizerName                         = "llm.redhat.com/environment-cleanup"
	NamespacePrefix                       = "env-"
	EnvironmentOwnerAnnotation            = "llm.redhat.com/environment-owner"
	EnvironmentUIDAnnotation              = "llm.redhat.com/environment-uid"
	AllowedClusterQueuesAnnotation        = "llm.redhat.com/allowed-cluster-queues"
	AccessBindingName                     = "environment-access"
	EnvironmentRoleName                   = "llm-workload-environment-user"
	VClusterRoleName                      = "llm-workload-environment-vcluster"
	NamespaceKubeconfigServiceAccountName = "environment-kubeconfig"
	NamespaceKubeconfigTokenSecretName    = "environment-kubeconfig-token"
	LocalQueueName                        = "default"
	DefaultClusterQueueName               = "inference-workspaces"
	KueueManagedLabel                     = "kueue.openshift.io/managed"
	VClusterKubeconfigKey                 = "config"
)

var ErrNamespaceCollision = errors.New("environment namespace already exists and is not controlled by this environment")
var ErrRouteCollision = errors.New("vCluster Route already exists and is not controlled by this environment")
var ErrVClusterBindingCollision = errors.New("vCluster ClusterRoleBinding already exists and is not controlled by this environment")
var ErrAccessBindingCollision = errors.New("environment access RoleBinding already exists and is not controlled by this environment")
var ErrLocalQueueCollision = errors.New("environment LocalQueue already exists and is not controlled by this environment")
var ErrNamespaceKubeconfigServiceAccountCollision = errors.New("environment kubeconfig ServiceAccount already exists and is not controlled by this environment")
var ErrNamespaceKubeconfigTokenSecretCollision = errors.New("environment kubeconfig token Secret already exists and is not controlled by this environment")

var openShiftRouteGVK = schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
var openShiftIngressGVK = schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "Ingress"}
var openShiftInfrastructureGVK = schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "Infrastructure"}

type LLMWorkloadEnvironmentReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Kueue             KueueClient
	VClusterInstaller environmentvcluster.Installer
	OpenShift         bool
	APIServerURL      string
}

func (r *LLMWorkloadEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := r.Get(ctx, req.NamespacedName, environment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	namespaceName := NamespacePrefix + environment.Name
	if environment.DeletionTimestamp != nil {
		return r.finalize(ctx, environment, namespaceName)
	}

	if !controllerutil.ContainsFinalizer(environment, FinalizerName) {
		controllerutil.AddFinalizer(environment, FinalizerName)
		if err := r.Update(ctx, environment); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if err := r.removeLegacyKubeconfigSecret(ctx, environment); err != nil {
		return ctrl.Result{}, err
	}

	reason, message, err := r.validateClusterQueue(ctx, environment)
	if err != nil {
		if errors.Is(err, ErrKueueAPIUnavailable) {
			return r.notReady(ctx, environment, namespaceName, "KueueAPINotAvailable", err.Error(), 30*time.Second, nil)
		}
		return r.notReady(ctx, environment, namespaceName, "ReconciliationFailed", err.Error(), 0, err)
	}
	if reason != "" {
		return r.notReady(ctx, environment, namespaceName, reason, message, 30*time.Second, nil)
	}

	if err := r.ensureNamespace(ctx, environment, namespaceName); err != nil {
		if errors.Is(err, ErrNamespaceCollision) {
			return r.notReady(ctx, environment, namespaceName, "NamespaceCollision", err.Error(), 30*time.Second, nil)
		}
		return r.notReady(ctx, environment, namespaceName, "ReconciliationFailed", err.Error(), 0, err)
	}
	if environmentMode(environment) == environmentv1alpha1.EnvironmentModeNamespace {
		if err := r.ensureNamespaceKubeconfigServiceAccount(ctx, environment, namespaceName); err != nil {
			return r.notReady(ctx, environment, namespaceName, "ReconciliationFailed", err.Error(), 0, err)
		}
	}
	if environmentMode(environment) == environmentv1alpha1.EnvironmentModeNamespace {
		if err := r.ensureNamespaceServiceAccountAccess(ctx, environment, namespaceName); err != nil {
			return r.notReady(ctx, environment, namespaceName, "ReconciliationFailed", err.Error(), 0, err)
		}
	} else if err := r.removeLegacyVClusterAccess(ctx, environment, namespaceName); err != nil {
		return r.notReady(ctx, environment, namespaceName, "ReconciliationFailed", err.Error(), 0, err)
	}

	queueActive, err := r.ensureLocalQueue(ctx, environment, namespaceName)
	if err != nil {
		if errors.Is(err, ErrKueueAPIUnavailable) {
			return r.notReady(ctx, environment, namespaceName, "KueueAPINotAvailable", err.Error(), 30*time.Second, nil)
		}
		return r.notReady(ctx, environment, namespaceName, "ReconciliationFailed", err.Error(), 0, err)
	}
	if !queueActive {
		return r.notReady(ctx, environment, namespaceName, "QueueNotReady", "Default LocalQueue is waiting for its ClusterQueue", 5*time.Second, nil)
	}

	var kubeconfig []byte
	if environmentMode(environment) == environmentv1alpha1.EnvironmentModeVCluster {
		config, ready, err := r.ensureVCluster(ctx, environment, namespaceName)
		if err != nil {
			return r.notReady(ctx, environment, namespaceName, "ReconciliationFailed", err.Error(), 0, err)
		}
		if !ready {
			return r.notReady(ctx, environment, namespaceName, "VClusterNotReady", "vCluster control plane is starting", 5*time.Second, nil)
		}
		kubeconfig = config
	} else {
		config, ready, err := r.ensureNamespaceKubeconfig(ctx, environment, namespaceName)
		if err != nil {
			return r.notReady(ctx, environment, namespaceName, "ReconciliationFailed", err.Error(), 0, err)
		}
		if !ready {
			return r.notReady(ctx, environment, namespaceName, "KubeconfigNotReady", "Service account token Secret is still being populated", 5*time.Second, nil)
		}
		kubeconfig = config
	}

	if err := r.setReady(ctx, environment, namespaceName, kubeconfig, metav1.ConditionTrue, "Ready", "Environment is ready"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *LLMWorkloadEnvironmentReconciler) ensureNamespace(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	name string,
) error {
	namespace := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, namespace)
	if apierrors.IsNotFound(err) {
		namespace = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Labels:      environmentNamespaceLabels(environment),
				Annotations: environmentOwnershipAnnotations(environment),
			},
		}
		return r.Create(ctx, namespace)
	}
	if err != nil {
		return err
	}
	if !environmentOwns(namespace, environment) {
		return fmt.Errorf("%w: %s", ErrNamespaceCollision, name)
	}
	if namespace.Labels == nil {
		namespace.Labels = make(map[string]string)
	}
	changed := false
	for key, value := range environmentNamespaceLabels(environment) {
		if namespace.Labels[key] != value {
			namespace.Labels[key] = value
			changed = true
		}
	}
	for key, value := range environmentOwnershipAnnotations(environment) {
		if namespace.Annotations == nil || namespace.Annotations[key] != value {
			if namespace.Annotations == nil {
				namespace.Annotations = make(map[string]string)
			}
			namespace.Annotations[key] = value
			changed = true
		}
	}
	if changed {
		return r.Update(ctx, namespace)
	}
	return nil
}

func environmentNamespaceLabels(environment *environmentv1alpha1.LLMWorkloadEnvironment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "llm-workload-environment-operator",
		"llm.redhat.com/environment":   environment.Name,
		KueueManagedLabel:              "true",
	}
}

func environmentOwnershipAnnotations(environment *environmentv1alpha1.LLMWorkloadEnvironment) map[string]string {
	return map[string]string{
		EnvironmentOwnerAnnotation: environment.Namespace + "/" + environment.Name,
		EnvironmentUIDAnnotation:   string(environment.UID),
	}
}

func environmentOwns(object metav1.Object, environment *environmentv1alpha1.LLMWorkloadEnvironment) bool {
	annotations := object.GetAnnotations()
	return annotations[EnvironmentOwnerAnnotation] == environment.Namespace+"/"+environment.Name &&
		annotations[EnvironmentUIDAnnotation] == string(environment.UID)
}

func markEnvironmentOwned(object metav1.Object, environment *environmentv1alpha1.LLMWorkloadEnvironment) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	for key, value := range environmentOwnershipAnnotations(environment) {
		annotations[key] = value
	}
	object.SetAnnotations(annotations)
}

func (r *LLMWorkloadEnvironmentReconciler) ensureNamespaceServiceAccountAccess(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
) error {
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: AccessBindingName, Namespace: namespace}}
	if err := r.Get(ctx, types.NamespacedName{Name: AccessBindingName, Namespace: namespace}, binding); err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if err == nil && !environmentOwns(binding, environment) {
		return fmt.Errorf("%w: %s/%s", ErrAccessBindingCollision, namespace, AccessBindingName)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: EnvironmentRoleName}
		binding.Subjects = []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: NamespaceKubeconfigServiceAccountName, Namespace: namespace,
		}}
		markEnvironmentOwned(binding, environment)
		return nil
	})
	return err
}

func (r *LLMWorkloadEnvironmentReconciler) removeLegacyVClusterAccess(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
) error {
	binding := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Name: AccessBindingName, Namespace: namespace}, binding); err == nil {
		if environmentOwns(binding, environment) {
			if err := r.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	role := &rbacv1.Role{}
	if err := r.Get(ctx, types.NamespacedName{Name: "environment-kubeconfig-reader", Namespace: namespace}, role); err == nil {
		if environmentOwns(role, environment) {
			if err := r.Delete(ctx, role); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *LLMWorkloadEnvironmentReconciler) legacyKubeconfigSecretName(environment *environmentv1alpha1.LLMWorkloadEnvironment) string {
	if environmentMode(environment) == environmentv1alpha1.EnvironmentModeNamespace {
		return "environment-" + environment.Name + "-kubeconfig"
	}
	if r.OpenShift && environmentMode(environment) == environmentv1alpha1.EnvironmentModeVCluster {
		return "vc-" + environment.Name + "-external"
	}
	return "vc-" + environment.Name
}

func (r *LLMWorkloadEnvironmentReconciler) removeLegacyKubeconfigSecret(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
) error {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: r.legacyKubeconfigSecretName(environment), Namespace: environment.Namespace}
	if err := r.Get(ctx, key, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !environmentOwns(secret, environment) {
		return nil
	}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy kubeconfig Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

func (r *LLMWorkloadEnvironmentReconciler) ensureNamespaceKubeconfigServiceAccount(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
) error {
	serviceAccount := &corev1.ServiceAccount{}
	key := types.NamespacedName{Name: NamespaceKubeconfigServiceAccountName, Namespace: namespace}
	if err := r.Get(ctx, key, serviceAccount); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		serviceAccount = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: NamespaceKubeconfigServiceAccountName, Namespace: namespace,
		}}
	} else if !environmentOwns(serviceAccount, environment) {
		return fmt.Errorf("%w: %s/%s", ErrNamespaceKubeconfigServiceAccountCollision, namespace, NamespaceKubeconfigServiceAccountName)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, serviceAccount, func() error {
		markEnvironmentOwned(serviceAccount, environment)
		return nil
	})
	return err
}

func (r *LLMWorkloadEnvironmentReconciler) ensureNamespaceKubeconfig(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
) ([]byte, bool, error) {
	tokenSecret := &corev1.Secret{}
	tokenKey := types.NamespacedName{Name: NamespaceKubeconfigTokenSecretName, Namespace: namespace}
	if err := r.Get(ctx, tokenKey, tokenSecret); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, err
		}
		tokenSecret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      NamespaceKubeconfigTokenSecretName,
			Namespace: namespace,
			Annotations: map[string]string{
				corev1.ServiceAccountNameKey: NamespaceKubeconfigServiceAccountName,
			},
		}, Type: corev1.SecretTypeServiceAccountToken}
	} else if !environmentOwns(tokenSecret, environment) {
		return nil, false, fmt.Errorf("%w: %s/%s", ErrNamespaceKubeconfigTokenSecretCollision, namespace, NamespaceKubeconfigTokenSecretName)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, tokenSecret, func() error {
		if tokenSecret.Annotations == nil {
			tokenSecret.Annotations = make(map[string]string)
		}
		tokenSecret.Annotations[corev1.ServiceAccountNameKey] = NamespaceKubeconfigServiceAccountName
		tokenSecret.Type = corev1.SecretTypeServiceAccountToken
		markEnvironmentOwned(tokenSecret, environment)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if len(tokenSecret.Data[corev1.ServiceAccountTokenKey]) == 0 || len(tokenSecret.Data[corev1.ServiceAccountRootCAKey]) == 0 {
		return nil, false, nil
	}
	server, err := r.clusterAPIServerURL(ctx)
	if err != nil {
		return nil, false, err
	}
	kubeconfig, err := buildServiceAccountKubeconfig(
		server,
		tokenSecret.Data[corev1.ServiceAccountRootCAKey],
		tokenSecret.Data[corev1.ServiceAccountTokenKey],
		environment.Namespace+"/"+environment.Name,
		NamespaceKubeconfigServiceAccountName,
	)
	if err != nil {
		return nil, false, err
	}
	return kubeconfig, true, nil
}

func (r *LLMWorkloadEnvironmentReconciler) clusterAPIServerURL(ctx context.Context) (string, error) {
	if r.OpenShift {
		infrastructure := &unstructured.Unstructured{}
		infrastructure.SetGroupVersionKind(openShiftInfrastructureGVK)
		if err := r.Get(ctx, types.NamespacedName{Name: "cluster"}, infrastructure); err == nil {
			if url, found, err := unstructured.NestedString(infrastructure.Object, "status", "apiServerURL"); err != nil {
				return "", err
			} else if found && url != "" {
				return strings.TrimRight(url, "/"), nil
			}
		} else if !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	if r.APIServerURL == "" {
		return "", errors.New("cluster API server URL is not configured")
	}
	return strings.TrimRight(r.APIServerURL, "/"), nil
}

func (r *LLMWorkloadEnvironmentReconciler) ensureClusterQueueExists(ctx context.Context, environment *environmentv1alpha1.LLMWorkloadEnvironment) error {
	if r.Kueue == nil {
		return fmt.Errorf("%w: Kueue client is not configured", ErrKueueAPIUnavailable)
	}
	return r.Kueue.EnsureClusterQueueExists(ctx, requestedClusterQueue(environment))
}

func (r *LLMWorkloadEnvironmentReconciler) validateClusterQueue(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
) (string, string, error) {
	landingNamespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: environment.Namespace}, landingNamespace); err != nil {
		if apierrors.IsNotFound(err) {
			return "LandingNamespaceNotFound", fmt.Sprintf("Landing namespace %q does not exist", environment.Namespace), nil
		}
		return "", "", err
	}

	requestedQueue := string(requestedClusterQueue(environment))
	if !clusterQueueAllowed(landingNamespace.Annotations[AllowedClusterQueuesAnnotation], requestedQueue) {
		return "ClusterQueueNotAllowed", fmt.Sprintf(
			"ClusterQueue %q is not allowed in landing namespace %q; annotate it with %s containing this queue",
			requestedQueue,
			environment.Namespace,
			AllowedClusterQueuesAnnotation,
		), nil
	}

	if err := r.ensureClusterQueueExists(ctx, environment); err != nil {
		if errors.Is(err, ErrKueueAPIUnavailable) {
			return "KueueAPINotAvailable", err.Error(), nil
		}
		if apierrors.IsNotFound(err) {
			return "ClusterQueueNotFound", fmt.Sprintf("ClusterQueue %q does not exist", requestedQueue), nil
		}
		return "", "", err
	}
	return "", "", nil
}

func clusterQueueAllowed(annotation, requestedQueue string) bool {
	for _, queue := range strings.Split(annotation, ",") {
		if strings.TrimSpace(queue) == requestedQueue {
			return true
		}
	}
	return false
}

func (r *LLMWorkloadEnvironmentReconciler) ensureLocalQueue(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
) (bool, error) {
	if r.Kueue == nil {
		return false, fmt.Errorf("%w: Kueue client is not configured", ErrKueueAPIUnavailable)
	}
	return r.Kueue.EnsureLocalQueue(ctx, environment, namespace, LocalQueueName)
}

func requestedClusterQueue(environment *environmentv1alpha1.LLMWorkloadEnvironment) string {
	if environment.Spec.ClusterQueue == "" {
		return DefaultClusterQueueName
	}
	return environment.Spec.ClusterQueue
}

func environmentMode(environment *environmentv1alpha1.LLMWorkloadEnvironment) environmentv1alpha1.EnvironmentMode {
	if environment.Spec.Mode == "" {
		return environmentv1alpha1.EnvironmentModeNamespace
	}
	return environment.Spec.Mode
}

func (r *LLMWorkloadEnvironmentReconciler) ensureVCluster(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespace string,
) ([]byte, bool, error) {
	if r.VClusterInstaller == nil {
		return nil, false, errors.New("vCluster installer is not configured")
	}
	bindingName := environment.Name + "-vcluster"
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: bindingName}}
	if err := r.Get(ctx, types.NamespacedName{Name: bindingName}, binding); err != nil && !apierrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("get vCluster CSI capacity binding: %w", err)
	} else if err == nil && !environmentOwns(binding, environment) {
		return nil, false, fmt.Errorf("%w: %s", ErrVClusterBindingCollision, bindingName)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: VClusterRoleName}
		binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: "vc-" + environment.Name, Namespace: namespace}}
		markEnvironmentOwned(binding, environment)
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("ensure vCluster CSI capacity binding: %w", err)
	}
	publicHost := ""
	if r.OpenShift {
		publicHost, err = r.openShiftVClusterHost(ctx, environment)
		if err != nil {
			return nil, false, fmt.Errorf("discover OpenShift vCluster hostname: %w", err)
		}
	}
	if err := r.VClusterInstaller.Ensure(ctx, environment.Name, namespace, r.OpenShift, publicHost); err != nil {
		return nil, false, err
	}

	statefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: environment.Name, Namespace: namespace}, statefulSet); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if statefulSet.Status.ReadyReplicas < 1 {
		return nil, false, nil
	}
	secretName := "vc-" + environment.Name
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	secretKey := kubeconfigSecretKey(secret)
	if secretKey == "" {
		return nil, false, fmt.Errorf("vCluster Secret %s/%s does not contain a kubeconfig in %q or %q", secret.Namespace, secret.Name, VClusterKubeconfigKey, "kubeconfig")
	}
	kubeconfig := append([]byte(nil), secret.Data[secretKey]...)
	if r.OpenShift {
		kubeconfig, err = r.ensureOpenShiftVClusterEndpoint(ctx, environment, namespace, kubeconfig)
		if err != nil {
			return nil, false, err
		}
	}
	return kubeconfig, true, nil
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

func kubeconfigSecretKey(secret *corev1.Secret) string {
	if _, found := secret.Data[VClusterKubeconfigKey]; found {
		return VClusterKubeconfigKey
	}
	if _, found := secret.Data["kubeconfig"]; found {
		return "kubeconfig"
	}
	return ""
}

func (r *LLMWorkloadEnvironmentReconciler) notReady(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespaceName string,
	reason string,
	message string,
	requeueAfter time.Duration,
	reconcileErr error,
) (ctrl.Result, error) {
	if err := r.setReady(ctx, environment, namespaceName, nil, metav1.ConditionFalse, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, reconcileErr
}

func (r *LLMWorkloadEnvironmentReconciler) setReady(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespaceName string,
	kubeconfig []byte,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	before := environment.DeepCopy()
	environment.Status.NamespaceRef = &corev1.LocalObjectReference{Name: namespaceName}
	environment.Status.Kubeconfig = append([]byte(nil), kubeconfig...)
	apimeta.SetStatusCondition(&environment.Status.Conditions, metav1.Condition{
		Type:               environmentv1alpha1.ReadyCondition,
		Status:             status,
		ObservedGeneration: environment.Generation,
		Reason:             reason,
		Message:            message,
	})
	return r.Status().Patch(ctx, environment, client.MergeFrom(before))
}

func (r *LLMWorkloadEnvironmentReconciler) finalize(
	ctx context.Context,
	environment *environmentv1alpha1.LLMWorkloadEnvironment,
	namespaceName string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(environment, FinalizerName) {
		return ctrl.Result{}, nil
	}

	if err := r.removeLegacyKubeconfigSecret(ctx, environment); err != nil {
		return ctrl.Result{}, err
	}

	binding := &rbacv1.ClusterRoleBinding{}
	bindingName := environment.Name + "-vcluster"
	err := r.Get(ctx, types.NamespacedName{Name: bindingName}, binding)
	if err == nil && environmentOwns(binding, environment) {
		if err := r.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	namespace := &corev1.Namespace{}
	err = r.Get(ctx, types.NamespacedName{Name: namespaceName}, namespace)
	if err == nil && environmentOwns(namespace, environment) {
		if namespace.DeletionTimestamp == nil {
			if err := r.Delete(ctx, namespace); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(environment, FinalizerName)
	return ctrl.Result{}, r.Update(ctx, environment)
}

func (r *LLMWorkloadEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&environmentv1alpha1.LLMWorkloadEnvironment{}).
		Complete(r)
}
