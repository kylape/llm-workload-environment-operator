package controller

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	envapi "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func hyperShiftTestProfile() HyperShiftProfile {
	p := HyperShiftProfile{ReleaseImage: "quay.io/example/release@sha256:" + strings.Repeat("a", 64), PullSecret: corev1.SecretReference{Name: "pull", Namespace: "operator"}, BaseDomain: "apps.example.com", APIServerAddress: "192.0.2.10", EtcdStorageClass: "lvm", WorkerStorageClass: "lvm"}
	p.defaults()
	return p
}

func hyperShiftFixture(t *testing.T) (*LLMWorkloadEnvironmentReconciler, *envapi.LLMWorkloadEnvironment) {
	t.Helper()
	scheme := testScheme(t)
	for _, gvk := range []schema.GroupVersionKind{hostedClusterGVK, nodePoolGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	e := &envapi.LLMWorkloadEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "requests", UID: "environment-uid", Generation: 1}, Spec: envapi.LLMWorkloadEnvironmentSpec{Mode: envapi.EnvironmentModeHyperShift}}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull", Namespace: "operator"}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"example.com":{"auth":"test"}}}`)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(e, hyperShiftObject(hostedClusterGVK, "", ""), hyperShiftObject(nodePoolGVK, "", "")).WithObjects(e, source).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if o.GetUID() == "" {
			o.SetUID(types.UID(o.GetName() + "-uid"))
		}
		o.SetGeneration(1)
		return c.Create(ctx, o, opts...)
	}}).Build()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(e), e); err != nil {
		t.Fatal(err)
	}
	return &LLMWorkloadEnvironmentReconciler{Client: c, APIReader: c, Scheme: scheme, OpenShift: true, HyperShift: &HyperShiftConfig{Enabled: true, Profiles: map[string]HyperShiftProfile{"default": hyperShiftTestProfile()}}}, e
}

func TestHyperShiftLifecycle(t *testing.T) {
	ctx := context.Background()
	r, e := hyperShiftFixture(t)
	if _, ready, err := r.ensureHyperShift(ctx, e, "env-test"); err != nil || ready {
		t.Fatalf("creation ready=%v err=%v", ready, err)
	}
	if e.Status.HostedClusterRef == nil || e.Status.NodePoolRef == nil || e.Status.HostedClusterRef.UID == "" {
		t.Fatal("missing durable provider identities")
	}
	key := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "env-test", Name: "hypershift-etcd-key"}, key); err != nil {
		t.Fatal(err)
	}
	firstKey := string(key.Data["key"])
	if len(firstKey) != 32 || key.Immutable == nil || !*key.Immutable {
		t.Fatal("AES key must be a persisted immutable 32-byte key")
	}
	if _, _, err := r.ensureHyperShift(ctx, e, "env-test"); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(key), key); err != nil {
		t.Fatal(err)
	}
	if string(key.Data["key"]) != firstKey {
		t.Fatal("idempotent reconcile rotated encryption key")
	}
	hc := hyperShiftObject(hostedClusterGVK, "env-test", "cluster")
	np := hyperShiftObject(nodePoolGVK, "env-test", "workers")
	if err := r.Get(ctx, client.ObjectKeyFromObject(hc), hc); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(hc.Object, "spec", "services", "port"); found {
		t.Fatal("API NodePort must be allocated by provider")
	}
	services, _, _ := unstructured.NestedSlice(hc.Object, "spec", "services")
	strategy := services[0].(map[string]interface{})["servicePublishingStrategy"].(map[string]interface{})["nodePort"].(map[string]interface{})
	if _, exists := strategy["port"]; exists {
		t.Fatal("fixed API port would collide between clusters")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(np), np); err != nil {
		t.Fatal(err)
	}
	if len(np.GetOwnerReferences()) != 1 || np.GetOwnerReferences()[0].UID != hc.GetUID() {
		t.Fatal("worker pool must belong to its hosted cluster")
	}
	// The API probe uses a real TLS endpoint and authenticated nodes request.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer guest-token" {
			t.Error("guest probe did not use guest credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/readyz" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		if req.URL.Path != "/api/v1/nodes" {
			t.Errorf("unexpected guest API path %s", req.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(&corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}, Items: []corev1.Node{{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}}})
	}))
	defer server.Close()
	host, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.ParseInt(portText, 10, 64)
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "guest"
	config.Contexts["guest"] = &clientcmdapi.Context{Cluster: "guest", AuthInfo: "guest"}
	config.Clusters["guest"] = &clientcmdapi.Cluster{Server: server.URL, CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
	config.AuthInfos["guest"] = &clientcmdapi.AuthInfo{Token: "guest-token"}
	data, _ := clientcmd.Write(*config)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "guest-admin", Namespace: "env-test", OwnerReferences: []metav1.OwnerReference{{APIVersion: hc.GetAPIVersion(), Kind: hc.GetKind(), Name: hc.GetName(), UID: hc.GetUID()}}}, Data: map[string][]byte{"kubeconfig": data}}
	if err := r.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	hc.Object["status"] = map[string]interface{}{"kubeconfig": map[string]interface{}{"name": secret.Name}, "controlPlaneEndpoint": map[string]interface{}{"host": host, "port": port}, "conditions": readyHyperShiftConditions(1, "Available", "ClusterVersionAvailable")}
	np.Object["status"] = map[string]interface{}{"replicas": int64(1), "conditions": readyHyperShiftConditions(1, "Ready", "AllNodesHealthy")}
	if err := r.Status().Update(ctx, hc); err != nil {
		t.Fatal(err)
	}
	if err := r.Status().Update(ctx, np); err != nil {
		t.Fatal(err)
	}
	if data, ready, err := r.ensureHyperShift(ctx, e, "env-test"); err != nil || !ready || len(data) == 0 {
		t.Fatalf("guest access ready=%v err=%v", ready, err)
	}
	// Scaling must withdraw readiness before the new worker is observed.
	two := int32(2)
	e.Spec.HyperShift = &envapi.HyperShiftEnvironmentSpec{Replicas: &two}
	if data, ready, err := r.ensureHyperShift(ctx, e, "env-test"); err != nil || ready || len(data) != 0 {
		t.Fatalf("scaling retained stale readiness: %v %v", ready, err)
	}
	if _, ready, err := r.ensureHyperShift(ctx, e, "env-test"); err != nil || ready {
		t.Fatalf("old status accepted after scaling: %v %v", ready, err)
	}
	if done, err := r.finalizeHyperShift(ctx, e, "env-test"); err != nil || done {
		t.Fatalf("must delete workers before cluster: %v %v", done, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(hc), hc); err != nil {
		t.Fatal("cluster removed before workers")
	}
	if done, err := r.finalizeHyperShift(ctx, e, "env-test"); err != nil || done {
		t.Fatalf("cluster deletion: %v %v", done, err)
	}
	cp := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "env-test-cluster"}}
	if err := r.Create(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if done, err := r.finalizeHyperShift(ctx, e, "env-test"); err != nil || done {
		t.Fatalf("must wait for provider namespace: %v %v", done, err)
	}
	if err := r.Delete(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if done, err := r.finalizeHyperShift(ctx, e, "env-test"); err != nil || !done {
		t.Fatalf("cleanup incomplete: %v %v", done, err)
	}
}

func readyHyperShiftConditions(generation int64, names ...string) []interface{} {
	out := []interface{}{}
	for _, name := range names {
		out = append(out, map[string]interface{}{"type": name, "status": "True", "observedGeneration": generation})
	}
	return out
}

func TestHyperShiftIdentityAndPolicyFailures(t *testing.T) {
	for _, test := range []string{"disabled", "unknown-profile", "replica-limit", "foreign-cluster", "foreign-key", "missing-key", "replaced-cluster", "missing-cluster", "profile-drift"} {
		t.Run(test, func(t *testing.T) {
			r, e := hyperShiftFixture(t)
			ctx := context.Background()
			if strings.HasPrefix(test, "foreign-") {
				if test == "foreign-cluster" {
					if err := r.Create(ctx, hyperShiftObject(hostedClusterGVK, "env-test", "cluster")); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := r.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "hypershift-etcd-key", Namespace: "env-test"}, Data: map[string][]byte{"key": make([]byte, 32)}}); err != nil {
						t.Fatal(err)
					}
				}
			} else if test != "disabled" && test != "unknown-profile" && test != "replica-limit" {
				if _, _, err := r.ensureHyperShift(ctx, e, "env-test"); err != nil {
					t.Fatal(err)
				}
			}
			switch test {
			case "disabled":
				r.HyperShift.Enabled = false
			case "unknown-profile":
				e.Spec.HyperShift = &envapi.HyperShiftEnvironmentSpec{Profile: "unapproved"}
			case "replica-limit":
				replicas := int32(11)
				e.Spec.HyperShift = &envapi.HyperShiftEnvironmentSpec{Replicas: &replicas}
			case "missing-key":
				if err := r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "hypershift-etcd-key", Namespace: "env-test"}}); err != nil {
					t.Fatal(err)
				}
			case "replaced-cluster":
				e.Status.HostedClusterRef.UID = "prior-identity"
			case "missing-cluster":
				if err := r.Delete(ctx, hyperShiftObject(hostedClusterGVK, "env-test", "cluster")); err != nil {
					t.Fatal(err)
				}
			case "profile-drift":
				p := r.HyperShift.Profiles["default"]
				p.WorkerMemory = "32Gi"
				r.HyperShift.Profiles["default"] = p
			}
			if config, ready, err := r.ensureHyperShift(ctx, e, "env-test"); err == nil || ready || len(config) != 0 {
				t.Fatalf("unsafe state accepted ready=%v err=%v", ready, err)
			}
			if test == "foreign-cluster" {
				if done, err := r.finalizeHyperShift(ctx, e, "env-test"); err == nil || done {
					t.Fatal("cleanup accepted foreign cluster")
				}
			}
		})
	}
}

func TestHyperShiftProfileConfiguration(t *testing.T) {
	p := hyperShiftTestProfile()
	good, _ := json.Marshal(HyperShiftConfig{Enabled: true, Profiles: map[string]HyperShiftProfile{"default": p}})
	for _, test := range []struct {
		name, value string
		valid       bool
	}{{"disabled", "", true}, {"valid", string(good), true}, {"empty", `{"enabled":true}`, false}, {"unknown", `{"enabled":false,"extra":true}`, false}, {"trailing", string(good) + ` {}`, false}} {
		t.Run(test.name, func(t *testing.T) {
			_, err := HyperShiftConfigFromEnv(func(string) string { return test.value })
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
		})
	}
	for _, change := range []func(*HyperShiftProfile){func(p *HyperShiftProfile) { p.ReleaseImage = "release:latest" }, func(p *HyperShiftProfile) { p.APIServerAddress = "127.0.0.1" }, func(p *HyperShiftProfile) { p.APIServerAddress = "api.example.com:6443" }, func(p *HyperShiftProfile) { p.PullSecret.Name = "" }, func(p *HyperShiftProfile) { p.WorkerRootDisk = "0" }, func(p *HyperShiftProfile) { p.ServiceNetwork = p.ClusterNetwork }, func(p *HyperShiftProfile) { p.ClusterNetwork = "10.244.0.0/24" }} {
		bad := p
		change(&bad)
		if bad.validate() == nil {
			t.Fatalf("invalid profile accepted: %s", fmt.Sprint(bad))
		}
	}
}

func TestHyperShiftCredentialsRejectUnsafeConfiguration(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	host, text, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.ParseInt(text, 10, 64)
	c := clientcmdapi.NewConfig()
	c.CurrentContext = "guest"
	c.Contexts["guest"] = &clientcmdapi.Context{Cluster: "guest", AuthInfo: "guest"}
	c.Clusters["guest"] = &clientcmdapi.Cluster{Server: server.URL, CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
	c.AuthInfos["guest"] = &clientcmdapi.AuthInfo{Token: "token"}
	for _, test := range []string{"valid", "exec", "ca-file", "insecure", "wrong-endpoint", "token-file", "impersonation"} {
		t.Run(test, func(t *testing.T) {
			bad := c.DeepCopy()
			switch test {
			case "exec":
				bad.AuthInfos["guest"].Exec = &clientcmdapi.ExecConfig{Command: "must-not-execute"}
			case "ca-file":
				bad.Clusters["guest"].CertificateAuthority = "/operator/credentials"
			case "insecure":
				bad.Clusters["guest"].InsecureSkipTLSVerify = true
			case "wrong-endpoint":
				bad.Clusters["guest"].Server = "https://host.example.com:6443"
			case "token-file":
				bad.AuthInfos["guest"].TokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
			case "impersonation":
				bad.AuthInfos["guest"].Impersonate = "admin"
			}
			data, _ := clientcmd.Write(*bad)
			_, err := portableHyperShiftKubeconfig(data, host, port)
			if (err == nil) != (test == "valid") {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}

func TestHyperShiftDisabledNeverPublishesHostCredentials(t *testing.T) {
	r, e := hyperShiftFixture(t)
	r.HyperShift.Enabled = false
	r.KueueAdmission = &KueueAdmissionConfig{Enabled: false}
	ctx := context.Background()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e)}
	for range 2 {
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Get(ctx, request.NamespacedName, e); err != nil {
		t.Fatal(err)
	}
	if len(e.Status.Kubeconfig) != 0 {
		t.Fatal("disabled hosted provider exposed a host kubeconfig")
	}
	found := false
	for _, condition := range e.Status.Conditions {
		if condition.Type == envapi.ReadyCondition {
			found = true
			if condition.Status != metav1.ConditionFalse || condition.Reason != "HyperShiftProvisioningFailed" {
				t.Fatalf("unexpected status: %v", condition)
			}
		}
	}
	if !found {
		t.Fatal("missing readiness status")
	}
	accounts := &corev1.ServiceAccountList{}
	if err := r.List(ctx, accounts, client.InNamespace("env-test")); err != nil {
		t.Fatal(err)
	}
	if len(accounts.Items) != 0 {
		t.Fatal("host service account created for hosted cluster mode")
	}
}

func TestHyperShiftIngressDomainsAreUnique(t *testing.T) {
	p := hyperShiftTestProfile()
	e := &envapi.LLMWorkloadEnvironment{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 51), UID: "first"}}
	first, _ := hyperShiftResources(e, "env-first", p, 1)
	e.UID = "second"
	second, _ := hyperShiftResources(e, "env-second", p, 1)
	a, _, _ := unstructured.NestedString(first.Object, "spec", "dns", "baseDomain")
	b, _, _ := unstructured.NestedString(second.Object, "spec", "dns", "baseDomain")
	if _, exists, _ := unstructured.NestedString(first.Object, "spec", "dns", "baseDomainPrefix"); exists {
		t.Fatal("HyperShift passthrough ignores baseDomainPrefix; use its default cluster-name prefix")
	}
	if a == b || strings.Split(a, ".")[0] == strings.Split(b, ".")[0] || len(strings.Split(a, ".")[0]) > 63 {
		t.Fatalf("invalid or colliding ingress prefixes: %q %q", a, b)
	}
}
