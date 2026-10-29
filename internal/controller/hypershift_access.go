package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func (r *LLMWorkloadEnvironmentReconciler) hyperShiftKubeconfig(ctx context.Context, hc *unstructured.Unstructured) ([]byte, error) {
	name, _, _ := unstructured.NestedString(hc.Object, "status", "kubeconfig", "name")
	if name == "" {
		return nil, fmt.Errorf("HostedCluster has not published an administrator kubeconfig")
	}
	s := &corev1.Secret{}
	if err := r.hyperShiftReader().Get(ctx, types.NamespacedName{Namespace: hc.GetNamespace(), Name: name}, s); err != nil {
		return nil, err
	}
	owned := false
	for _, owner := range s.OwnerReferences {
		if owner.APIVersion == hostedClusterGVK.GroupVersion().String() && owner.Kind == hc.GetKind() && owner.Name == hc.GetName() && owner.UID != "" && owner.UID == hc.GetUID() {
			owned = true
		}
	}
	if !owned {
		return nil, fmt.Errorf("administrator kubeconfig Secret is not owned by the recorded HostedCluster")
	}
	host, _, _ := unstructured.NestedString(hc.Object, "status", "controlPlaneEndpoint", "host")
	port, _, _ := unstructured.NestedInt64(hc.Object, "status", "controlPlaneEndpoint", "port")
	return portableHyperShiftKubeconfig(s.Data["kubeconfig"], host, port)
}

// Publish a single portable context. Never execute credential plugins or read
// paths supplied by a provider Secret inside the privileged operator process.
func portableHyperShiftKubeconfig(data []byte, host string, port int64) ([]byte, error) {
	c, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("invalid hosted administrator kubeconfig")
	}
	context := c.Contexts[c.CurrentContext]
	if context == nil {
		return nil, fmt.Errorf("hosted kubeconfig has no current context")
	}
	cluster, auth := c.Clusters[context.Cluster], c.AuthInfos[context.AuthInfo]
	if cluster == nil || auth == nil {
		return nil, fmt.Errorf("hosted kubeconfig is missing its cluster or credentials")
	}
	u, err := url.Parse(cluster.Server)
	if err != nil || u.Scheme != "https" || u.Host != net.JoinHostPort(host, strconv.FormatInt(port, 10)) || host == "" || port < 1 || port > 65535 || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("kubeconfig endpoint does not match the published hosted API endpoint")
	}
	pool := x509.NewCertPool()
	if cluster.InsecureSkipTLSVerify || cluster.CertificateAuthority != "" || cluster.ProxyURL != "" || !pool.AppendCertsFromPEM(cluster.CertificateAuthorityData) {
		return nil, fmt.Errorf("hosted kubeconfig requires embedded trusted CA data")
	}
	if auth.Exec != nil || auth.AuthProvider != nil || auth.TokenFile != "" || auth.ClientCertificate != "" || auth.ClientKey != "" || auth.Username != "" || auth.Password != "" || auth.Impersonate != "" || auth.ImpersonateUID != "" || len(auth.ImpersonateGroups) != 0 || len(auth.ImpersonateUserExtra) != 0 {
		return nil, fmt.Errorf("hosted kubeconfig must use portable token or client certificate credentials")
	}
	if auth.Token == "" {
		if _, err := tls.X509KeyPair(auth.ClientCertificateData, auth.ClientKeyData); err != nil {
			return nil, fmt.Errorf("hosted kubeconfig requires valid client credentials")
		}
	}
	out := clientcmdapi.NewConfig()
	out.CurrentContext = "environment"
	out.Contexts["environment"] = &clientcmdapi.Context{Cluster: "environment", AuthInfo: "admin"}
	out.Clusters["environment"] = &clientcmdapi.Cluster{Server: cluster.Server, CertificateAuthorityData: cluster.CertificateAuthorityData, TLSServerName: cluster.TLSServerName}
	out.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: auth.Token, ClientCertificateData: auth.ClientCertificateData, ClientKeyData: auth.ClientKeyData}
	return clientcmd.Write(*out)
}

func probeHyperShiftAPI(ctx context.Context, data []byte, replicas int32) error {
	config, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return fmt.Errorf("hosted API client configuration is invalid")
	}
	config.Timeout = 5 * time.Second
	guest, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("could not create hosted API client")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := guest.CoreV1().RESTClient().Get().AbsPath("/readyz").Do(ctx).Error(); err != nil {
		return fmt.Errorf("hosted API is not ready")
	}
	nodes, err := guest.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("hosted API credentials cannot list worker nodes")
	}
	var ready int32
	for _, node := range nodes.Items {
		if _, master := node.Labels["node-role.kubernetes.io/master"]; master {
			continue
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue && !node.Spec.Unschedulable {
				ready++
			}
		}
	}
	if ready < replicas {
		return fmt.Errorf("hosted worker nodes are not ready")
	}
	return nil
}
