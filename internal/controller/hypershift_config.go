package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// HyperShiftConfig is administrator policy, never tenant-supplied manifests.
// Profiles keep releases, credentials, placement and worker capacity bounded.
type HyperShiftConfig struct {
	Enabled  bool                         `json:"enabled"`
	Profiles map[string]HyperShiftProfile `json:"profiles"`
}

type HyperShiftProfile struct {
	ReleaseImage                string                  `json:"releaseImage"`
	PullSecret                  corev1.SecretReference  `json:"pullSecret"`
	SSHKey                      *corev1.SecretReference `json:"sshKey,omitempty"`
	BaseDomain                  string                  `json:"baseDomain"`
	APIServerAddress            string                  `json:"apiServerAddress"`
	APIServerPublishingStrategy string                  `json:"apiServerPublishingStrategy,omitempty"`
	EtcdStorageClass            string                  `json:"etcdStorageClass"`
	EtcdStorageSize             string                  `json:"etcdStorageSize,omitempty"`
	WorkerStorageClass          string                  `json:"workerStorageClass"`
	WorkerCores                 int32                   `json:"workerCores,omitempty"`
	WorkerMemory                string                  `json:"workerMemory,omitempty"`
	WorkerRootDisk              string                  `json:"workerRootDisk,omitempty"`
	MaxReplicas                 int32                   `json:"maxReplicas,omitempty"`
	ControlPlaneNodeSelector    map[string]string       `json:"controlPlaneNodeSelector,omitempty"`
	WorkerNodeSelector          map[string]string       `json:"workerNodeSelector,omitempty"`
	ClusterNetwork              string                  `json:"clusterNetwork,omitempty"`
	ServiceNetwork              string                  `json:"serviceNetwork,omitempty"`
}

func HyperShiftConfigFromEnv(getenv func(string) string) (*HyperShiftConfig, error) {
	c := &HyperShiftConfig{}
	if value := strings.TrimSpace(getenv("HYPERSHIFT_CONFIG")); value != "" {
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(c); err != nil {
			return nil, fmt.Errorf("HYPERSHIFT_CONFIG must be a valid profile configuration: %w", err)
		}
		if err := decoder.Decode(new(interface{})); err != io.EOF {
			return nil, fmt.Errorf("HYPERSHIFT_CONFIG must contain one JSON object")
		}
	}
	if c.Enabled && len(c.Profiles) == 0 {
		return nil, fmt.Errorf("HyperShift requires at least one administrator profile")
	}
	for name, p := range c.Profiles {
		if len(validation.IsDNS1123Label(name)) != 0 {
			return nil, fmt.Errorf("invalid HyperShift profile name")
		}
		p.defaults()
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("HyperShift profile %q: %w", name, err)
		}
		c.Profiles[name] = p
	}
	return c, nil
}

func (p *HyperShiftProfile) defaults() {
	if p.EtcdStorageSize == "" {
		p.EtcdStorageSize = "8Gi"
	}
	if p.WorkerCores == 0 {
		p.WorkerCores = 16
	}
	if p.WorkerMemory == "" {
		p.WorkerMemory = "128Gi"
	}
	if p.WorkerRootDisk == "" {
		p.WorkerRootDisk = "258Gi"
	}
	if p.MaxReplicas == 0 {
		p.MaxReplicas = 10
	}
	if p.ClusterNetwork == "" {
		p.ClusterNetwork = "10.244.0.0/16"
	}
	if p.ServiceNetwork == "" {
		p.ServiceNetwork = "172.29.0.0/16"
	}
}

func (p HyperShiftProfile) validate() error {
	if !regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`).MatchString(p.ReleaseImage) {
		return fmt.Errorf("releaseImage must be digest-pinned")
	}
	if len(validation.IsDNS1123Subdomain(p.BaseDomain)) != 0 || p.BaseDomain == "" {
		return fmt.Errorf("baseDomain must be a DNS name")
	}
	if p.APIServerPublishingStrategy != "" && p.APIServerPublishingStrategy != "NodePort" && p.APIServerPublishingStrategy != "Route" {
		return fmt.Errorf("apiServerPublishingStrategy must be NodePort or Route")
	}
	if p.APIServerPublishingStrategy != "Route" {
		if ip := net.ParseIP(p.APIServerAddress); ip != nil {
			if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
				return fmt.Errorf("apiServerAddress must be reachable by hosted workers and clients")
			}
		} else if len(validation.IsDNS1123Subdomain(p.APIServerAddress)) != 0 || p.APIServerAddress == "localhost" || strings.HasSuffix(p.APIServerAddress, ".svc") || strings.HasSuffix(p.APIServerAddress, ".cluster.local") {
			return fmt.Errorf("apiServerAddress must be an external node address without a port")
		}
	}
	for _, ref := range []*corev1.SecretReference{&p.PullSecret, p.SSHKey} {
		if ref != nil && (len(validation.IsDNS1123Label(ref.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(ref.Name)) != 0) {
			return fmt.Errorf("credential references require a namespace and Secret name")
		}
	}
	for _, class := range []string{p.EtcdStorageClass, p.WorkerStorageClass} {
		if len(validation.IsDNS1123Subdomain(class)) != 0 {
			return fmt.Errorf("storage classes must be configured")
		}
	}
	for _, value := range []string{p.EtcdStorageSize, p.WorkerMemory, p.WorkerRootDisk} {
		q, err := resource.ParseQuantity(value)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("storage and memory must be positive quantities")
		}
	}
	if p.WorkerCores < 1 || p.WorkerCores > 256 || p.MaxReplicas < 1 || p.MaxReplicas > 100 {
		return fmt.Errorf("workerCores must be 1..256 and maxReplicas 1..100")
	}
	for _, selector := range []map[string]string{p.ControlPlaneNodeSelector, p.WorkerNodeSelector} {
		for k, v := range selector {
			if len(validation.IsQualifiedName(k)) != 0 || len(validation.IsValidLabelValue(v)) != 0 {
				return fmt.Errorf("invalid placement label")
			}
		}
	}
	var networks []*net.IPNet
	for index, value := range []string{p.ClusterNetwork, p.ServiceNetwork} {
		ip, network, err := net.ParseCIDR(value)
		if err != nil || ip.To4() == nil || ip.String() != network.IP.String() {
			return fmt.Errorf("guest networks must be canonical IPv4 CIDRs")
		}
		if prefix, _ := network.Mask.Size(); index == 0 && prefix > 23 {
			return fmt.Errorf("clusterNetwork must accommodate the default /23 node subnet")
		}
		for _, other := range networks {
			if network.Contains(other.IP) || other.Contains(network.IP) {
				return fmt.Errorf("guest networks overlap")
			}
		}
		networks = append(networks, network)
	}
	return nil
}
