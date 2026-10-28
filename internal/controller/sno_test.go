package controller

import (
	"context"
	"testing"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func snoFixture(t *testing.T) (*LLMWorkloadEnvironmentReconciler, *environmentv1alpha1.LLMWorkloadEnvironment, ctrl.Request) {
	t.Helper()
	scheme := testScheme(t)
	scheme.AddKnownTypeWithName(snoGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(snoGVK.GroupVersion().WithKind("SNOVirtualMachineList"), &unstructured.UnstructuredList{})
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "sno-example", Namespace: "requests", UID: types.UID("environment-uid"), Finalizers: []string{FinalizerName}}, Spec: environmentv1alpha1.LLMWorkloadEnvironmentSpec{Mode: environmentv1alpha1.EnvironmentModeSNO}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(environment, newSNO("unused")).WithObjects(environment).Build()
	r := &LLMWorkloadEnvironmentReconciler{Client: c, Scheme: scheme, OpenShift: true, KueueAdmission: &KueueAdmissionConfig{Enabled: false}}
	return r, environment, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(environment)}
}

func snoKubeconfig(t *testing.T) []byte {
	t.Helper()
	config := clientcmdapi.Config{CurrentContext: "sno", Clusters: map[string]*clientcmdapi.Cluster{"sno": {Server: "https://api.sno.example.test", CertificateAuthorityData: []byte("test-only-ca")}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"sno": {Token: "test-only-token"}}, Contexts: map[string]*clientcmdapi.Context{"sno": {Cluster: "sno", AuthInfo: "sno"}}}
	b, err := clientcmd.Write(config)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSNORequestIsIdempotentAndDoesNotPublishHostCredentials(t *testing.T) {
	r, e, request := snoFixture(t)
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	child := newSNO(NamespacePrefix + e.Name)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal(err)
	}
	if len(child.GetOwnerReferences()) != 0 || !environmentOwns(child, e) {
		t.Fatal("SNO request has unsafe cross-namespace ownership")
	}
	profile, _, _ := unstructured.NestedString(child.Object, "spec", "imageProfile")
	if profile != "default" {
		t.Fatalf("profile: %s", profile)
	}
	namespace := &corev1.Namespace{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: child.GetNamespace()}, namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels["sno.klape.io/enabled"] != "true" {
		t.Fatal("SNO namespace policy marker missing")
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := r.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Kubeconfig) != 0 || current.Status.SNOVirtualMachineRef == nil {
		t.Fatal("waiting SNO must expose only the request reference")
	}
	if condition := apimeta.FindStatusCondition(current.Status.Conditions, environmentv1alpha1.ReadyCondition); condition == nil || condition.Reason != "SNOProvisioning" {
		t.Fatalf("condition: %+v", condition)
	}
	secrets := &corev1.SecretList{}
	if err := r.List(context.Background(), secrets, client.InNamespace(child.GetNamespace())); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 0 {
		t.Fatal("SNO mode created namespace-token credentials")
	}
}

func TestSNOExternalCredentialsRequireCurrentReadinessAndOwnedSecret(t *testing.T) {
	r, e, request := snoFixture(t)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	child := newSNO(NamespacePrefix + e.Name)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal(err)
	}
	child.SetUID(types.UID("provider-uid"))
	child.SetGeneration(2)
	if err := r.Update(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	status := map[string]interface{}{"apiEndpoint": "https://api.sno.example.test", "credentialsSecretRef": map[string]interface{}{"name": "sno-access"}, "conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "observedGeneration": int64(1)}}}
	child.Object["status"] = status
	if err := r.Status().Update(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := r.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Kubeconfig) != 0 {
		t.Fatal("stale provider readiness was accepted")
	}
	status["conditions"] = []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "observedGeneration": int64(2)}}
	child.Object["status"] = status
	if err := r.Status().Update(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sno-access", Namespace: child.GetNamespace()}, Data: map[string][]byte{"kubeconfig": snoKubeconfig(t)}}
	if err := r.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), request); err == nil {
		t.Fatal("unowned access Secret accepted")
	}
	secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: snoGVK.GroupVersion().String(), Kind: snoGVK.Kind, Name: child.GetName(), UID: child.GetUID()}}
	if err := r.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	result, err := r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if !apimeta.IsStatusConditionTrue(current.Status.Conditions, environmentv1alpha1.ReadyCondition) || string(current.Status.Kubeconfig) != string(secret.Data["kubeconfig"]) || result.RequeueAfter == 0 {
		t.Fatal("provider access was not handed off and polled")
	}
}

func TestSNORejectsWrongEndpointInsecureTLSAndExternalCredentialCommands(t *testing.T) {
	for _, which := range []string{"endpoint", "insecure", "exec", "file-ca"} {
		t.Run(which, func(t *testing.T) {
			config, err := clientcmd.Load(snoKubeconfig(t))
			if err != nil {
				t.Fatal(err)
			}
			switch which {
			case "endpoint":
				config.Clusters["sno"].Server = "https://internal.svc:6443"
			case "insecure":
				config.Clusters["sno"].InsecureSkipTLSVerify = true
			case "exec":
				config.AuthInfos["sno"].Exec = &clientcmdapi.ExecConfig{Command: "untrusted", APIVersion: "client.authentication.k8s.io/v1"}
			case "file-ca":
				config.Clusters["sno"].CertificateAuthority = "/tmp/controller-only-ca"
			}
			data, err := clientcmd.Write(*config)
			if err != nil {
				t.Fatal(err)
			}
			if validateSNOAccess(data, "https://api.sno.example.test") == nil {
				t.Fatal("unsafe provider credentials accepted")
			}
		})
	}
}

func TestSNOFinalizationWaitsBeforeNamespaceDeletion(t *testing.T) {
	r, e, request := snoFixture(t)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	child := newSNO(NamespacePrefix + e.Name)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal(err)
	}
	child.SetUID(types.UID("provider-uid"))
	child.SetFinalizers([]string{"sno.klape.io/cleanup"})
	if err := r.Update(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := r.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal(err)
	}
	if child.GetDeletionTimestamp() == nil {
		t.Fatal("SNO cleanup was not requested")
	}
	if err := r.Get(context.Background(), client.ObjectKey{Name: child.GetNamespace()}, &corev1.Namespace{}); err != nil {
		t.Fatal("backing namespace removed before SNO cleanup", err)
	}
	child.SetFinalizers(nil)
	if err := r.Update(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKey{Name: child.GetNamespace()}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatal("namespace was not deleted after provider cleanup", err)
	}
}
