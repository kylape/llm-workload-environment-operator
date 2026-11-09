package controller

import (
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// assertKubeconfigIdentity verifies that a published kubeconfig names its
// context, cluster, and user after the LWE and selects that context.
func assertKubeconfigIdentity(t *testing.T, data []byte, name string) {
	t.Helper()
	config, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("load published kubeconfig: %v", err)
	}
	if config.CurrentContext != name {
		t.Fatalf("current context %q, want %q", config.CurrentContext, name)
	}
	if len(config.Contexts) != 1 || config.Contexts[name] == nil {
		t.Fatalf("contexts must contain exactly %q: %+v", name, config.Contexts)
	}
	context := config.Contexts[name]
	if context.Cluster != name || context.AuthInfo != name {
		t.Fatalf("context must reference cluster and user %q: %+v", name, context)
	}
	if len(config.Clusters) != 1 || config.Clusters[name] == nil {
		t.Fatalf("clusters must contain exactly %q: %+v", name, config.Clusters)
	}
	if len(config.AuthInfos) != 1 || config.AuthInfos[name] == nil {
		t.Fatalf("users must contain exactly %q: %+v", name, config.AuthInfos)
	}
}

func renameTestKubeconfig(t *testing.T) []byte {
	t.Helper()
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "environment"
	config.Contexts["environment"] = &clientcmdapi.Context{Cluster: "environment", AuthInfo: "admin"}
	config.Clusters["environment"] = &clientcmdapi.Cluster{Server: "https://api.example.test", CertificateAuthorityData: []byte("test-only-ca")}
	config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "test-only-token"}
	data, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRenameKubeconfigIdentity(t *testing.T) {
	published, err := renameKubeconfigIdentity(renameTestKubeconfig(t), "my-openshift")
	if err != nil {
		t.Fatal(err)
	}
	assertKubeconfigIdentity(t, published, "my-openshift")
	config, err := clientcmd.Load(published)
	if err != nil {
		t.Fatal(err)
	}
	if config.Clusters["my-openshift"].Server != "https://api.example.test" ||
		string(config.Clusters["my-openshift"].CertificateAuthorityData) != "test-only-ca" {
		t.Fatalf("cluster fields not preserved: %+v", config.Clusters["my-openshift"])
	}
	if config.AuthInfos["my-openshift"].Token != "test-only-token" {
		t.Fatalf("credentials not preserved: %+v", config.AuthInfos["my-openshift"])
	}
	// Renames are stable across repeated publish cycles.
	again, err := renameKubeconfigIdentity(published, "my-openshift")
	if err != nil {
		t.Fatal(err)
	}
	assertKubeconfigIdentity(t, again, "my-openshift")
}

func TestRenameKubeconfigIdentityRejectsInvalidNames(t *testing.T) {
	for _, name := range []string{"", "requests/test", "My-OpenShift", "a_b", " spaces "} {
		if _, err := renameKubeconfigIdentity(renameTestKubeconfig(t), name); err == nil {
			t.Fatalf("invalid kubeconfig identity %q accepted", name)
		}
	}
}

func TestRenameKubeconfigIdentityRejectsIncompleteConfigs(t *testing.T) {
	data := renameTestKubeconfig(t)
	config, err := clientcmd.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	delete(config.Clusters, "environment")
	broken, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renameKubeconfigIdentity(broken, "test"); err == nil {
		t.Fatal("kubeconfig with a missing cluster accepted")
	}
	if _, err := renameKubeconfigIdentity([]byte("not a kubeconfig"), "test"); err == nil {
		t.Fatal("invalid kubeconfig accepted")
	}
}
