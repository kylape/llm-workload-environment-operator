package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	envapi "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func sharedIngressFixture(t *testing.T) (*LLMWorkloadEnvironmentReconciler, *envapi.LLMWorkloadEnvironment, HyperShiftProfile) {
	r, e := hyperShiftFixture(t)
	for _, gvk := range []schema.GroupVersionKind{certificateGVK, issuerGVK, hostedControlPlaneGVK, openShiftRouteGVK} {
		r.Scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		r.Scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	p := hyperShiftTestProfile()
	p.APIServerPublishingStrategy = "SharedIngress"
	r.HyperShift.Profiles["default"] = p
	return r, e, p
}

func testAPISecrets(t *testing.T, e *envapi.LLMWorkloadEnvironment, host string) (*corev1.Secret, *corev1.Secret, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test API CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caKey, _ := x509.MarshalECPrivateKey(key)
	root := testAPISecret(e, "hypershift-api-ca", caPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKey}), caPEM)
	leaf := testAPILeaf(t, e, host, ca, key)
	return root, leaf, ca, key
}

func testAPILeaf(t *testing.T, e *envapi.LLMWorkloadEnvironment, host string, ca *x509.Certificate, caKey *ecdsa.PrivateKey) *corev1.Secret {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return testAPISecret(e, hyperShiftAPISecret, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))
}

func testAPISecret(e *envapi.LLMWorkloadEnvironment, name string, cert, key, ca []byte) *corev1.Secret {
	annotations := environmentOwnershipAnnotations(e)
	annotations["cert-manager.io/certificate-name"] = name
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "env-test", Annotations: annotations}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: key, "ca.crt": ca}}
}

func readyTestCertificate(t *testing.T, r *LLMWorkloadEnvironmentReconciler, name string, stale bool) {
	t.Helper()
	c := hyperShiftObject(certificateGVK, "env-test", name)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(c), c); err != nil {
		t.Fatal(err)
	}
	generation := c.GetGeneration()
	if stale {
		generation--
	}
	c.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "observedGeneration": generation}}}
	if err := r.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func noHostedCluster(t *testing.T, r *LLMWorkloadEnvironmentReconciler) {
	t.Helper()
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "env-test", Name: "cluster"}, hyperShiftObject(hostedClusterGVK, "", "")); !apierrors.IsNotFound(err) {
		t.Fatalf("HostedCluster created before certificate ready: %v", err)
	}
}

func TestHyperShiftCertificateSequencingAndRoute(t *testing.T) {
	ctx := context.Background()
	r, e, p := sharedIngressFixture(t)
	reconcile := func() {
		t.Helper()
		if _, ready, err := r.ensureHyperShift(ctx, e, "env-test"); err != nil || ready {
			t.Fatalf("ready=%v err=%v", ready, err)
		}
	}
	reconcile()
	noHostedCluster(t, r)
	readyTestCertificate(t, r, "hypershift-api-ca", false)
	reconcile()
	noHostedCluster(t, r) // Ready status alone is insufficient without the TLS Secret.
	root, leaf, _, _ := testAPISecrets(t, e, hyperShiftAPIHost(e, p))
	if err := r.Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	reconcile()
	noHostedCluster(t, r)
	readyTestCertificate(t, r, hyperShiftAPISecret, true)
	if err := r.Create(ctx, leaf); err != nil {
		t.Fatal(err)
	}
	reconcile()
	noHostedCluster(t, r) // Stale Ready must not unblock creation.
	readyTestCertificate(t, r, hyperShiftAPISecret, false)
	reconcile()
	hc := hyperShiftObject(hostedClusterGVK, "env-test", "cluster")
	if err := r.Get(ctx, client.ObjectKeyFromObject(hc), hc); err != nil {
		t.Fatal(err)
	}
	host, _, _ := unstructured.NestedString(hc.Object, "spec", "kubeAPIServerDNSName")
	certs, _, _ := unstructured.NestedSlice(hc.Object, "spec", "configuration", "apiServer", "servingCerts", "namedCertificates")
	if host != hyperShiftAPIHost(e, p) || len(certs) != 1 {
		t.Fatal("HostedCluster lacks issued certificate configuration")
	}
	services, _, _ := unstructured.NestedSlice(hc.Object, "spec", "services")
	if services[0].(map[string]interface{})["servicePublishingStrategy"].(map[string]interface{})["type"] != "NodePort" {
		t.Fatal("worker NodePort endpoint changed")
	}
	hcp := hyperShiftObject(hostedControlPlaneGVK, "env-test-cluster", "cluster")
	hcp.SetAnnotations(map[string]string{"hypershift.openshift.io/cluster": "env-test/cluster"})
	infra, _, _ := unstructured.NestedString(hc.Object, "spec", "infraID")
	hcp.Object["spec"] = map[string]interface{}{"infraID": infra}
	if err := r.Create(ctx, hcp); err != nil {
		t.Fatal(err)
	}
	reconcile()
	route := hyperShiftObject(openShiftRouteGVK, "env-test-cluster", "lwe-api")
	if err := r.Get(ctx, client.ObjectKeyFromObject(route), route); err != nil {
		t.Fatal(err)
	}
	if _, found := route.GetLabels()["hypershift.openshift.io/hosted-control-plane"]; found {
		t.Fatal("Route selects dedicated router")
	}
	if len(route.GetOwnerReferences()) != 1 || route.GetOwnerReferences()[0].UID != hcp.GetUID() {
		t.Fatal("Route lacks HCP lifecycle owner")
	}
	route.Object["status"] = map[string]interface{}{"ingress": []interface{}{map[string]interface{}{"host": host, "conditions": []interface{}{map[string]interface{}{"type": "Admitted", "status": "True"}}}}}
	if err := r.Update(ctx, route); err != nil {
		t.Fatal(err)
	}
	if ready, err := r.ensureHyperShiftAPIRoute(ctx, e, hc, p); err != nil || !ready {
		t.Fatalf("admitted Route ready=%v err=%v", ready, err)
	}
	route.SetAnnotations(nil)
	if err := r.Update(ctx, route); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ensureHyperShiftAPIRoute(ctx, e, hc, p); err == nil {
		t.Fatal("foreign Route accepted")
	}
}

func TestHyperShiftRejectsForeignCertificateResources(t *testing.T) {
	for _, kind := range []string{"Issuer", "Certificate", "Secret"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, e, _ := sharedIngressFixture(t)
			var o client.Object
			switch kind {
			case "Issuer":
				o = hyperShiftObject(issuerGVK, "env-test", "hypershift-api-selfsigned")
				o.(*unstructured.Unstructured).Object["spec"] = map[string]interface{}{"selfSigned": map[string]interface{}{}}
			case "Certificate":
				o = hyperShiftCertificate(e, "env-test", "hypershift-api-ca", "hypershift-api-selfsigned")
			case "Secret":
				o = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "hypershift-api-ca", Namespace: "env-test"}}
			}
			if err := r.Create(ctx, o); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.ensureHyperShift(ctx, e, "env-test"); err == nil {
				t.Fatal("foreign certificate resource accepted")
			}
			noHostedCluster(t, r)
		})
	}
}

func TestHyperShiftIngressKubeconfigAndRenewal(t *testing.T) {
	ctx := context.Background()
	r, e, p := sharedIngressFixture(t)
	_, leaf, ca, key := testAPISecrets(t, e, hyperShiftAPIHost(e, p))
	if err := r.Create(ctx, leaf); err != nil {
		t.Fatal(err)
	}
	c := clientcmdapi.NewConfig()
	c.CurrentContext = "guest"
	c.Contexts["guest"] = &clientcmdapi.Context{Cluster: "guest", AuthInfo: "admin"}
	c.Clusters["guest"] = &clientcmdapi.Cluster{Server: "https://192.0.2.10:31234", CertificateAuthorityData: []byte("old-provider-ca"), TLSServerName: "old-provider-host"}
	c.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "guest-admin-token"}
	raw, _ := clientcmd.Write(*c)
	hc := hyperShiftObject(hostedClusterGVK, "env-test", "cluster")
	first, err := r.hyperShiftIngressKubeconfig(ctx, e, hc, p, raw)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := clientcmd.Load(first)
	if out.Clusters["environment"].Server != "https://"+hyperShiftAPIHost(e, p)+":443" || out.Clusters["environment"].InsecureSkipTLSVerify || out.Clusters["environment"].TLSServerName != "" || out.AuthInfos["admin"].Token != "guest-admin-token" {
		t.Fatal("invalid ingress kubeconfig")
	}
	if string(out.Clusters["environment"].CertificateAuthorityData) != string(leaf.Data["ca.crt"]) {
		t.Fatal("kubeconfig does not trust issuer CA")
	}
	renewed := testAPILeaf(t, e, hyperShiftAPIHost(e, p), ca, key)
	leaf.Data = renewed.Data
	if err := r.Update(ctx, leaf); err != nil {
		t.Fatal(err)
	}
	second, err := r.hyperShiftIngressKubeconfig(ctx, e, hc, p, raw)
	if err != nil || string(first) != string(second) {
		t.Fatalf("leaf renewal changed stable CA trust: %v", err)
	}
	wrong := testAPILeaf(t, e, "wrong.example.com", ca, key)
	leaf.Data = wrong.Data
	if err := r.Update(ctx, leaf); err != nil {
		t.Fatal(err)
	}
	if _, err := r.hyperShiftIngressKubeconfig(ctx, e, hc, p, raw); err == nil {
		t.Fatal("wrong hostname certificate accepted")
	}
}
