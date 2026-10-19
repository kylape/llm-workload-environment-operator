package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func testArgoCDKubeconfig() *clientcmdapi.Config {
	return &clientcmdapi.Config{
		CurrentContext: "selected",
		Contexts:       map[string]*clientcmdapi.Context{"selected": {Cluster: "public", AuthInfo: "tenant"}},
		Clusters: map[string]*clientcmdapi.Cluster{
			"public": {Server: "https://203.0.113.10:443", CertificateAuthorityData: []byte("test-ca"), TLSServerName: "api.example.test"},
			"unused": {Server: "https://unused.invalid"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"tenant": {Token: "private-tenant-token"}},
	}
}

func encodeArgoCDTestKubeconfig(t *testing.T, config *clientcmdapi.Config) []byte {
	t.Helper()
	raw, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newArgoCDTestEnvironment(t *testing.T) *environmentv1alpha1.LLMWorkloadEnvironment {
	t.Helper()
	return &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "example", Namespace: "requests", UID: types.UID("environment-uid"), Finalizers: []string{FinalizerName},
			Labels: map[string]string{ApplicationProfileLabel: "demo", "app.kubernetes.io/part-of": "scooby-demo", "app.kubernetes.io/instance": "foreign-app"},
		},
		Spec: environmentv1alpha1.LLMWorkloadEnvironmentSpec{Mode: environmentv1alpha1.EnvironmentModeVCluster},
		Status: environmentv1alpha1.LLMWorkloadEnvironmentStatus{
			Kubeconfig: encodeArgoCDTestKubeconfig(t, testArgoCDKubeconfig()),
			Conditions: []metav1.Condition{{Type: environmentv1alpha1.ReadyCondition, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()}},
		},
	}
}

func argoCDTestClient(t *testing.T, environment *environmentv1alpha1.LLMWorkloadEnvironment, objects ...client.Object) client.WithWatch {
	t.Helper()
	objects = append(objects, environment)
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(environment).WithObjects(objects...).Build()
}

func loadArgoCDTestEnvironment(t *testing.T, c client.Client, environment *environmentv1alpha1.LLMWorkloadEnvironment) *environmentv1alpha1.LLMWorkloadEnvironment {
	t.Helper()
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(environment), current); err != nil {
		t.Fatal(err)
	}
	return current
}

func TestBuildArgoCDClusterData(t *testing.T) {
	for _, certificate := range []bool{false, true} {
		t.Run(map[bool]string{false: "token", true: "certificate"}[certificate], func(t *testing.T) {
			config := testArgoCDKubeconfig()
			config.Clusters["public"].DisableCompression = true
			if certificate {
				config.AuthInfos["tenant"] = &clientcmdapi.AuthInfo{ClientCertificateData: []byte("test-cert"), ClientKeyData: []byte("test-key")}
			}
			data, err := buildArgoCDClusterData(encodeArgoCDTestKubeconfig(t, config), "lwe-test")
			if err != nil {
				t.Fatal(err)
			}
			if string(data["server"]) != config.Clusters["public"].Server || string(data["name"]) != "lwe-test" || len(data) != 3 {
				t.Fatal("registration did not select the current-context endpoint and identity")
			}
			var connection argoCDClusterConfig
			if err := json.Unmarshal(data["config"], &connection); err != nil {
				t.Fatal(err)
			}
			if string(connection.TLSClientConfig.CAData) != "test-ca" || connection.TLSClientConfig.Insecure ||
				connection.TLSClientConfig.ServerName != "api.example.test" || !connection.DisableCompression {
				t.Fatal("TLS settings or compression were lost or encoded incorrectly")
			}
			if certificate {
				if connection.BearerToken != "" || string(connection.TLSClientConfig.CertData) != "test-cert" || string(connection.TLSClientConfig.KeyData) != "test-key" {
					t.Fatal("client certificate authentication was not preserved")
				}
			} else if connection.BearerToken != "private-tenant-token" {
				t.Fatal("bearer token was not preserved")
			}
		})
	}
}

func TestBuildArgoCDClusterDataRejectsUnsupportedCredentials(t *testing.T) {
	cases := map[string]func(*clientcmdapi.Config){
		"no-current-context": func(c *clientcmdapi.Config) { c.CurrentContext = "" },
		"unknown-context":    func(c *clientcmdapi.Config) { c.CurrentContext = "missing" },
		"unknown-cluster":    func(c *clientcmdapi.Config) { c.Contexts["selected"].Cluster = "missing" },
		"unknown-user":       func(c *clientcmdapi.Config) { c.Contexts["selected"].AuthInfo = "missing" },
		"http":               func(c *clientcmdapi.Config) { c.Clusters["public"].Server = "http://api.example.test" },
		"invalid-url":        func(c *clientcmdapi.Config) { c.Clusters["public"].Server = "https://[private-tenant-token" },
		"url-credentials": func(c *clientcmdapi.Config) {
			c.Clusters["public"].Server = "https://user:private-tenant-token@api.example.test"
		},
		"url-query":        func(c *clientcmdapi.Config) { c.Clusters["public"].Server += "?token=private-tenant-token" },
		"ca-file":          func(c *clientcmdapi.Config) { c.Clusters["public"].CertificateAuthority = "/private-tenant-token" },
		"certificate-file": func(c *clientcmdapi.Config) { c.AuthInfos["tenant"].ClientCertificate = "/private-tenant-token" },
		"key-file":         func(c *clientcmdapi.Config) { c.AuthInfos["tenant"].ClientKey = "/private-tenant-token" },
		"token-file":       func(c *clientcmdapi.Config) { c.AuthInfos["tenant"].TokenFile = "/private-tenant-token" },
		"exec": func(c *clientcmdapi.Config) {
			c.AuthInfos["tenant"].Exec = &clientcmdapi.ExecConfig{Command: "/private-tenant-token"}
		},
		"auth-provider": func(c *clientcmdapi.Config) {
			c.AuthInfos["tenant"].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "private-tenant-token"}
		},
		"basic-auth":        func(c *clientcmdapi.Config) { c.AuthInfos["tenant"].Password = "private-tenant-token" },
		"impersonation":     func(c *clientcmdapi.Config) { c.AuthInfos["tenant"].Impersonate = "private-tenant-token" },
		"impersonation-uid": func(c *clientcmdapi.Config) { c.AuthInfos["tenant"].ImpersonateUID = "private-tenant-token" },
		"impersonation-groups": func(c *clientcmdapi.Config) {
			c.AuthInfos["tenant"].ImpersonateGroups = []string{"private-tenant-token"}
		},
		"impersonation-extra": func(c *clientcmdapi.Config) {
			c.AuthInfos["tenant"].ImpersonateUserExtra = map[string][]string{"private-tenant-token": {"value"}}
		},
		"proxy": func(c *clientcmdapi.Config) { c.Clusters["public"].ProxyURL = "https://private-tenant-token" },
		"missing-key": func(c *clientcmdapi.Config) {
			c.AuthInfos["tenant"].ClientCertificateData = []byte("private-tenant-token")
		},
		"missing-certificate": func(c *clientcmdapi.Config) { c.AuthInfos["tenant"].ClientKeyData = []byte("private-tenant-token") },
		"anonymous":           func(c *clientcmdapi.Config) { c.AuthInfos["tenant"].Token = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := testArgoCDKubeconfig()
			mutate(config)
			_, err := buildArgoCDClusterData(encodeArgoCDTestKubeconfig(t, config), "lwe-test")
			if err == nil || strings.Contains(err.Error(), "private-tenant-token") {
				t.Fatal("unsupported config was accepted or error exposed input")
			}
		})
	}
	if _, err := buildArgoCDClusterData([]byte("users: private-tenant-token\n"), "test"); err == nil || strings.Contains(err.Error(), "private-tenant-token") {
		t.Fatal("decoder errors must be sanitized")
	}
}

func TestArgoCDClusterSecretSynchronization(t *testing.T) {
	ctx := context.Background()
	environment := newArgoCDTestEnvironment(t)
	c := argoCDTestClient(t, environment)
	environment = loadArgoCDTestEnvironment(t, c, environment)
	r := &LLMWorkloadEnvironmentReconciler{Client: c, ArgoCDNamespace: "argocd"}
	if err := r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Name: argoCDClusterSecretName(environment), Namespace: "argocd"}
	if err := c.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	wantLabels := map[string]string{ApplicationProfileLabel: "demo", ArgoCDSecretTypeLabel: "cluster"}
	if !reflect.DeepEqual(secret.Labels, wantLabels) || len(secret.OwnerReferences) != 0 || !environmentOwns(secret, environment) || secret.Type != corev1.SecretTypeOpaque {
		t.Fatal("unexpected registration labels, type, or ownership")
	}
	persisted := loadArgoCDTestEnvironment(t, c, environment)
	if persisted.Status.ArgoCDClusterSecretRef == nil || persisted.Status.ArgoCDClusterSecretRef.Name != secret.Name {
		t.Fatal("missing durable cleanup reference")
	}
	version := secret.ResourceVersion
	if err := r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, secret); err != nil || secret.ResourceVersion != version {
		t.Fatal("unchanged registration caused an update")
	}
	secret.Annotations["argocd.argoproj.io/skip-reconcile"] = "false"
	if err := c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	config := testArgoCDKubeconfig()
	config.AuthInfos["tenant"].Token = "rotated-token"
	config.Clusters["public"].Server = "https://203.0.113.20:443"
	environment.Labels[ApplicationProfileLabel] = "another-profile"
	if err := r.ensureArgoCDClusterSecret(ctx, environment, encodeArgoCDTestKubeconfig(t, config)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	var connection argoCDClusterConfig
	if err := json.Unmarshal(secret.Data["config"], &connection); err != nil {
		t.Fatal(err)
	}
	if connection.BearerToken != "rotated-token" || string(secret.Data["server"]) != config.Clusters["public"].Server ||
		secret.Labels[ApplicationProfileLabel] != "another-profile" || secret.Annotations["argocd.argoproj.io/skip-reconcile"] != "false" {
		t.Fatal("credentials, endpoint, profile, or unrelated metadata were not synchronized")
	}
	if err := c.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig); err != nil {
		t.Fatal("registration deletion was not repaired:", err)
	}
}

func TestArgoCDReferenceMustBePersistedBeforeSecretCreation(t *testing.T) {
	ctx := context.Background()
	environment := newArgoCDTestEnvironment(t)
	c := argoCDTestClient(t, environment)
	environment = loadArgoCDTestEnvironment(t, c, environment)
	creates := 0
	blocked := interceptor.NewClient(c, interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return errors.New("private-tenant-token")
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			creates++
			return nil
		},
	})
	r := &LLMWorkloadEnvironmentReconciler{Client: blocked, ArgoCDNamespace: "argocd"}
	err := r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig)
	if err == nil || creates != 0 || strings.Contains(err.Error(), "private-tenant-token") {
		t.Fatal("reference write failure must prevent creation and must be sanitized")
	}
	// Simulate a controller restart after recording intent but before creation.
	environment = loadArgoCDTestEnvironment(t, c, environment)
	r.Client = c
	if err := r.setArgoCDSecretReference(ctx, environment, &corev1.SecretReference{Name: argoCDClusterSecretName(environment), Namespace: "argocd"}); err != nil {
		t.Fatal(err)
	}
	environment = loadArgoCDTestEnvironment(t, c, environment)
	if err := r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig); err != nil {
		t.Fatal("recorded intent was not resumed:", err)
	}
}

func TestArgoCDNamespaceChangeAndDisableCleanup(t *testing.T) {
	ctx := context.Background()
	environment := newArgoCDTestEnvironment(t)
	c := argoCDTestClient(t, environment)
	environment = loadArgoCDTestEnvironment(t, c, environment)
	r := &LLMWorkloadEnvironmentReconciler{Client: c, ArgoCDNamespace: "old-argocd"}
	if err := r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig); err != nil {
		t.Fatal(err)
	}
	r.ArgoCDNamespace = "new-argocd"
	if err := r.prepareArgoCDRegistration(ctx, environment); !errors.Is(err, errArgoCDCleanupPending) {
		t.Fatalf("cleanup should await confirmed absence: %v", err)
	}
	if environment.Status.ArgoCDClusterSecretRef == nil || environment.Status.ArgoCDClusterSecretRef.Namespace != "old-argocd" {
		t.Fatal("cleanup reference cleared before absence was confirmed")
	}
	environment = loadArgoCDTestEnvironment(t, c, environment)
	if err := r.prepareArgoCDRegistration(ctx, environment); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig); err != nil {
		t.Fatal(err)
	}
	if environment.Status.ArgoCDClusterSecretRef.Namespace != "new-argocd" {
		t.Fatal("registration did not move")
	}
	r.ArgoCDNamespace = ""
	if err := r.prepareArgoCDRegistration(ctx, environment); !errors.Is(err, errArgoCDCleanupPending) {
		t.Fatal(err)
	}
	if err := r.prepareArgoCDRegistration(ctx, environment); err != nil || environment.Status.ArgoCDClusterSecretRef != nil {
		t.Fatal("disabling integration must clean up its last recorded namespace")
	}
}

func TestArgoCDCleanupWaitsForFinalizersAndUsesPreconditions(t *testing.T) {
	ctx := context.Background()
	environment := newArgoCDTestEnvironment(t)
	ref := &corev1.SecretReference{Name: argoCDClusterSecretName(environment), Namespace: "argocd"}
	environment.Status.ArgoCDClusterSecretRef = ref
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace, UID: "secret-uid", Finalizers: []string{"test/hold"}, Annotations: environmentOwnershipAnnotations(environment)}}
	c := argoCDTestClient(t, environment, secret)
	var checked bool
	checkedClient := interceptor.NewClient(c, interceptor.Funcs{Delete: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		options := &client.DeleteOptions{}
		for _, option := range opts {
			option.ApplyToDelete(options)
		}
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != obj.GetUID() ||
			options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != obj.GetResourceVersion() {
			t.Fatal("delete must protect object UID and resource version")
		}
		checked = true
		return underlying.Delete(ctx, obj, opts...)
	}})
	environment = loadArgoCDTestEnvironment(t, c, environment)
	r := &LLMWorkloadEnvironmentReconciler{Client: checkedClient}
	for i := 0; i < 2; i++ {
		if err := r.removeArgoCDClusterSecret(ctx, environment); !errors.Is(err, errArgoCDCleanupPending) {
			t.Fatal(err)
		}
	}
	if !checked || environment.Status.ArgoCDClusterSecretRef == nil {
		t.Fatal("cleanup did not retain its reference")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}
	secret.Finalizers = nil
	if err := c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := r.removeArgoCDClusterSecret(ctx, environment); err != nil || environment.Status.ArgoCDClusterSecretRef != nil {
		t.Fatal("cleanup should finish after finalizers release:", err)
	}
}

func TestArgoCDCollisionAndAPIErrors(t *testing.T) {
	ctx := context.Background()
	environment := newArgoCDTestEnvironment(t)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: argoCDClusterSecretName(environment), Namespace: "argocd"}, Data: map[string][]byte{"private": []byte("foreign")}}
	c := argoCDTestClient(t, environment, secret)
	environment = loadArgoCDTestEnvironment(t, c, environment)
	r := &LLMWorkloadEnvironmentReconciler{Client: c, ArgoCDNamespace: "argocd"}
	if err := r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig); !errors.Is(err, ErrArgoCDClusterSecretCollision) {
		t.Fatal(err)
	}
	if err := r.removeArgoCDClusterSecret(ctx, environment); !errors.Is(err, ErrArgoCDClusterSecretCollision) {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil || string(secret.Data["private"]) != "foreign" {
		t.Fatal("foreign Secret was changed")
	}
	if err := r.finalizeArgoCDRegistration(ctx, environment); err != nil || environment.Status.ArgoCDClusterSecretRef != nil {
		t.Fatal("foreign Secret must not prevent finalization")
	}
	if err := c.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	r.Client = interceptor.NewClient(c, interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "test", errors.New("private-tenant-token"))
	}})
	err := r.ensureArgoCDClusterSecret(ctx, environment, environment.Status.Kubeconfig)
	if err == nil || !strings.Contains(err.Error(), "Forbidden") || strings.Contains(err.Error(), "private-tenant-token") {
		t.Fatal("API error was not sanitized")
	}
}

func TestArgoCDSecretEvents(t *testing.T) {
	ctx := context.Background()
	environment := newArgoCDTestEnvironment(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: NamespacePrefix + environment.Name, Annotations: environmentOwnershipAnnotations(environment)}}
	c := argoCDTestClient(t, environment, namespace)
	r := &LLMWorkloadEnvironmentReconciler{Client: c, ArgoCDNamespace: "argocd"}
	registration := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: argoCDClusterSecretName(environment), Namespace: "argocd", Annotations: environmentOwnershipAnnotations(environment)}}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vc-" + environment.Name, Namespace: namespace.Name}}
	for _, secret := range []*corev1.Secret{registration, source} {
		requests := r.mapArgoCDSecret(ctx, secret)
		if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(environment) {
			t.Fatal("Secret event did not map to its environment")
		}
		pred := argoCDSecretPredicate()
		if !pred.Create(event.CreateEvent{Object: secret}) || !pred.Delete(event.DeleteEvent{Object: secret}) {
			t.Fatal("create/delete event was filtered out")
		}
		unchanged := secret.DeepCopy()
		unchanged.ResourceVersion = "new-version"
		if pred.Update(event.UpdateEvent{ObjectOld: secret, ObjectNew: unchanged}) {
			t.Fatal("version-only event caused a reconcile")
		}
		changed := secret.DeepCopy()
		changed.Data = map[string][]byte{"config": []byte("rotated")}
		if !pred.Update(event.UpdateEvent{ObjectOld: secret, ObjectNew: changed}) {
			t.Fatal("credential rotation was filtered out")
		}
	}
	unrelated := registration.DeepCopy()
	unrelated.Name = "unrelated"
	if len(r.mapArgoCDSecret(ctx, unrelated)) != 0 || argoCDSecretPredicate().Create(event.CreateEvent{Object: unrelated}) {
		t.Fatal("unrelated Secret enqueued an environment")
	}
	stale := registration.DeepCopy()
	stale.Annotations[EnvironmentUIDAnnotation] = "old-uid"
	if len(r.mapArgoCDSecret(ctx, stale)) != 0 {
		t.Fatal("stale UID event enqueued a replacement environment")
	}
	wrongNamespace := source.DeepCopy()
	wrongNamespace.Namespace = "another-namespace"
	if len(r.mapArgoCDSecret(ctx, wrongNamespace)) != 0 {
		t.Fatal("source Secret in wrong namespace was accepted")
	}
	if !argoCDSecretPredicate().Update(event.UpdateEvent{ObjectOld: registration, ObjectNew: stale}) {
		t.Fatal("ownership change was filtered out")
	}
}

func TestArgoCDRegistrationConditionStates(t *testing.T) {
	for _, tc := range []struct {
		name, target, reason string
		mutate               func(*environmentv1alpha1.LLMWorkloadEnvironment)
		provisionErr         error
		status               metav1.ConditionStatus
	}{
		{name: "registered", target: "argocd", reason: "Registered", status: metav1.ConditionTrue},
		{name: "not-requested", target: "argocd", reason: "NotRequested", status: metav1.ConditionFalse, mutate: func(e *environmentv1alpha1.LLMWorkloadEnvironment) { delete(e.Labels, ApplicationProfileLabel) }},
		{name: "disabled", reason: "IntegrationDisabled", status: metav1.ConditionFalse},
		{name: "namespace-mode", target: "argocd", reason: "UnsupportedMode", status: metav1.ConditionFalse, mutate: func(e *environmentv1alpha1.LLMWorkloadEnvironment) {
			e.Spec.Mode = environmentv1alpha1.EnvironmentModeNamespace
		}},
		{name: "waiting", target: "argocd", reason: "WaitingForEnvironment", status: metav1.ConditionFalse, mutate: func(e *environmentv1alpha1.LLMWorkloadEnvironment) { e.Status.Conditions = nil }},
		{name: "provisioning-error", target: "argocd", reason: "WaitingForEnvironment", status: metav1.ConditionFalse, provisionErr: errors.New("provisioning failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newArgoCDTestEnvironment(t)
			if tc.mutate != nil {
				tc.mutate(e)
			}
			c := argoCDTestClient(t, e)
			e = loadArgoCDTestEnvironment(t, c, e)
			r := &LLMWorkloadEnvironmentReconciler{Client: c, ArgoCDNamespace: tc.target}
			if err := r.reconcileArgoCDRegistration(context.Background(), e, nil, tc.provisionErr); err != nil {
				t.Fatal(err)
			}
			condition := apimeta.FindStatusCondition(e.Status.Conditions, environmentv1alpha1.ArgoCDRegisteredCondition)
			if condition == nil || condition.Status != tc.status || condition.Reason != tc.reason {
				t.Fatalf("unexpected registration condition: %#v", condition)
			}
			if tc.reason != "Registered" {
				secrets := &corev1.SecretList{}
				if err := c.List(context.Background(), secrets); err != nil {
					t.Fatal(err)
				}
				if len(secrets.Items) != 0 {
					t.Fatal("registration was published without a ready opted-in environment")
				}
			}
		})
	}
}

func TestValidateArgoCDNamespace(t *testing.T) {
	for _, namespace := range []string{"", "argocd", "llm-workload-environment-operator"} {
		if err := ValidateArgoCDNamespace(namespace); err != nil {
			t.Fatal(err)
		}
	}
	for _, namespace := range []string{" argocd", "ArgoCD", "a.b", strings.Repeat("a", 64), "../argocd"} {
		if ValidateArgoCDNamespace(namespace) == nil {
			t.Fatal("invalid namespace accepted")
		}
	}
}
