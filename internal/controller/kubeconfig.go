package controller

import (
	"encoding/base64"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

// renameKubeconfigIdentity republishes a validated kubeconfig with its
// current context, cluster, and user named after the LWE, and sets
// current-context to that name. Merging several LWE kubeconfigs via
// KUBECONFIG then cannot collide: identical generic names (for example
// "admin") would otherwise make the first file's definitions silently win,
// pointing one context at the wrong cluster with the wrong credentials.
// Only the current context's cluster and user are kept; their contents are
// preserved verbatim.
func renameKubeconfigIdentity(data []byte, name string) ([]byte, error) {
	if errs := validation.IsDNS1123Label(name); len(errs) != 0 {
		return nil, fmt.Errorf("kubeconfig identity %q is not a valid DNS-1123 label: %s", name, strings.Join(errs, ", "))
	}
	config, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("invalid kubeconfig")
	}
	if config.CurrentContext == "" {
		return nil, fmt.Errorf("kubeconfig has no current context")
	}
	context, ok := config.Contexts[config.CurrentContext]
	if !ok || context == nil {
		return nil, fmt.Errorf("kubeconfig current context %q is missing", config.CurrentContext)
	}
	cluster, ok := config.Clusters[context.Cluster]
	if !ok || cluster == nil {
		return nil, fmt.Errorf("kubeconfig cluster %q is missing", context.Cluster)
	}
	auth, ok := config.AuthInfos[context.AuthInfo]
	if !ok || auth == nil {
		return nil, fmt.Errorf("kubeconfig user %q is missing", context.AuthInfo)
	}
	out := clientcmdapi.NewConfig()
	out.CurrentContext = name
	out.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name, Namespace: context.Namespace}
	out.Clusters[name] = cluster.DeepCopy()
	out.AuthInfos[name] = auth.DeepCopy()
	return clientcmd.Write(*out)
}

func rewriteKubeconfigServer(raw []byte, server string) ([]byte, error) {
	config := map[string]interface{}{}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return nil, err
	}

	clusters, found := config["clusters"].([]interface{})
	if !found || len(clusters) == 0 {
		return nil, fmt.Errorf("kubeconfig has no clusters")
	}
	updated := false
	for _, item := range clusters {
		clusterEntry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		cluster, ok := clusterEntry["cluster"].(map[string]interface{})
		if !ok {
			continue
		}
		cluster["server"] = server
		updated = true
	}
	if !updated {
		return nil, fmt.Errorf("kubeconfig has no cluster entries")
	}

	return yaml.Marshal(config)
}

func buildServiceAccountKubeconfig(server string, caData, token []byte, clusterName, userName string) ([]byte, error) {
	if server == "" {
		return nil, fmt.Errorf("kubernetes API server URL is empty")
	}
	if len(caData) == 0 {
		return nil, fmt.Errorf("kubernetes CA data is empty")
	}
	if len(token) == 0 {
		return nil, fmt.Errorf("service account token is empty")
	}
	config := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []interface{}{
			map[string]interface{}{
				"name": clusterName,
				"cluster": map[string]interface{}{
					"server":                     server,
					"certificate-authority-data": base64.StdEncoding.EncodeToString(caData),
				},
			},
		},
		"users": []interface{}{
			map[string]interface{}{
				"name": userName,
				"user": map[string]interface{}{"token": string(token)},
			},
		},
		"contexts": []interface{}{
			map[string]interface{}{
				"name": clusterName,
				"context": map[string]interface{}{
					"cluster": clusterName,
					"user":    userName,
				},
			},
		},
		"current-context": clusterName,
	}
	return yaml.Marshal(config)
}
