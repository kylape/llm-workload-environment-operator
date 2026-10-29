package controller

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	envapi "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestHyperShiftEarlyAccessAndWithdrawal(t *testing.T) {
	ctx := context.Background()
	r, e := hyperShiftFixture(t)
	r.KueueAdmission = &KueueAdmissionConfig{Enabled: false}
	if _, _, err := r.ensureHyperShift(ctx, e, "env-test"); err != nil {
		t.Fatal(err)
	}
	hc := hyperShiftObject(hostedClusterGVK, "env-test", "cluster")
	np := hyperShiftObject(nodePoolGVK, "env-test", "workers")
	if err := r.Get(ctx, client.ObjectKeyFromObject(hc), hc); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(np), np); err != nil {
		t.Fatal(err)
	}
	var failed atomic.Bool
	var workersReady atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer guest-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if failed.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if req.URL.Path == "/readyz" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		if req.URL.Path != "/api/v1/nodes" {
			http.NotFound(w, req)
			return
		}
		nodes := []corev1.Node{}
		if workersReady.Load() {
			nodes = append(nodes, corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}, Items: nodes})
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
	// No Available/ClusterVersionAvailable conditions, worker conditions or nodes.
	hc.Object["status"] = map[string]interface{}{"kubeconfig": map[string]interface{}{"name": secret.Name}, "controlPlaneEndpoint": map[string]interface{}{"host": host, "port": port}}
	if err := r.Status().Update(ctx, hc); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e)}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		if err := r.Get(ctx, request.NamespacedName, e); err != nil {
			t.Fatal(err)
		}
	}
	reconcile() // Install finalizer.
	reconcile()
	assertStatus := func(apiAccessible, ready bool) {
		t.Helper()
		api := apimeta.FindStatusCondition(e.Status.Conditions, envapi.APIAccessibleCondition)
		full := apimeta.FindStatusCondition(e.Status.Conditions, envapi.ReadyCondition)
		if api == nil || full == nil || api.ObservedGeneration != e.Generation || full.ObservedGeneration != e.Generation {
			t.Fatal("missing current-generation access/readiness conditions")
		}
		if (api.Status == metav1.ConditionTrue) != apiAccessible || (full.Status == metav1.ConditionTrue) != ready || (len(e.Status.Kubeconfig) > 0) != apiAccessible {
			t.Fatalf("access=%v ready=%v credentials=%v", api, full, len(e.Status.Kubeconfig) > 0)
		}
	}
	assertStatus(true, false)
	failed.Store(true)
	reconcile()
	assertStatus(false, false)
	failed.Store(false)
	reconcile()
	assertStatus(true, false)
	// Healthy workers alone do not bypass full installation readiness.
	workersReady.Store(true)
	np.Object["status"] = map[string]interface{}{"replicas": int64(1), "conditions": readyHyperShiftConditions(1, "Ready", "AllNodesHealthy")}
	if err := r.Status().Update(ctx, np); err != nil {
		t.Fatal(err)
	}
	reconcile()
	assertStatus(true, false)
	hc.Object["status"].(map[string]interface{})["conditions"] = readyHyperShiftConditions(1, "Available", "ClusterVersionAvailable")
	if err := r.Status().Update(ctx, hc); err != nil {
		t.Fatal(err)
	}
	reconcile()
	assertStatus(true, true)
	// A replaced credential owner must withdraw published access immediately.
	secret.OwnerReferences[0].UID = "replacement-cluster-uid"
	if err := r.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	reconcile()
	assertStatus(false, false)
}
