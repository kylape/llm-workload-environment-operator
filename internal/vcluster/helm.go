package vcluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

const (
	DefaultHelmPath  = "/helm"
	DefaultChartPath = "/charts/vcluster-0.0.1-combined.4.tgz"
)

type Installer interface {
	Ensure(context.Context, string, string, bool, string) error
}

type HelmInstaller struct {
	HelmPath  string
	ChartPath string
}

type helmRelease struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type helmReleaseValues struct {
	ControlPlane struct {
		Proxy struct {
			ExtraSANs []string `json:"extraSANs"`
		} `json:"proxy"`
	} `json:"controlPlane"`
}

func (h HelmInstaller) Ensure(ctx context.Context, name, namespace string, openShift bool, publicHost string) error {
	helmPath := h.HelmPath
	if helmPath == "" {
		helmPath = DefaultHelmPath
	}
	chartPath := h.ChartPath
	if chartPath == "" {
		chartPath = DefaultChartPath
	}

	list, err := h.run(ctx, helmPath, "list", "--namespace", namespace, "--filter", "^"+name+"$", "--output", "json")
	if err != nil {
		return fmt.Errorf("list vCluster Helm releases: %w", err)
	}
	var releases []helmRelease
	if err := json.Unmarshal(list, &releases); err != nil {
		return fmt.Errorf("decode Helm release list: %w", err)
	}
	if releaseDeployed(releases) {
		if publicHost == "" {
			return nil
		}
		configured, err := h.releaseHasPublicHost(ctx, helmPath, name, namespace, publicHost)
		if err != nil {
			return fmt.Errorf("check vCluster Helm release public certificate SAN: %w", err)
		}
		if configured {
			return nil
		}
	}

	if _, err := h.run(ctx, helmPath, installArgs(name, namespace, chartPath, openShift, publicHost)...); err != nil {
		return fmt.Errorf("install vCluster Helm release: %w", err)
	}
	return nil
}

func (h HelmInstaller) releaseHasPublicHost(ctx context.Context, helmPath, name, namespace, publicHost string) (bool, error) {
	valuesJSON, err := h.run(ctx, helmPath, "get", "values", name, "--namespace", namespace, "--output", "json")
	if err != nil {
		return false, err
	}
	values := helmReleaseValues{}
	if err := json.Unmarshal(valuesJSON, &values); err != nil {
		return false, fmt.Errorf("decode Helm release values: %w", err)
	}
	for _, san := range values.ControlPlane.Proxy.ExtraSANs {
		if san == publicHost {
			return true, nil
		}
	}
	return false, nil
}

func releaseDeployed(releases []helmRelease) bool {
	return len(releases) == 1 && releases[0].Status == "deployed"
}

func installArgs(name, namespace, chartPath string, openShift bool, publicHost string) []string {
	args := []string{
		"upgrade", "--install", name, chartPath,
		"--namespace", namespace,
		"--set-string", "controlPlane.statefulSet.image.registry=quay.io",
		"--set-string", "controlPlane.statefulSet.image.repository=klape/vcluster",
		"--set-string", "controlPlane.statefulSet.image.tag=csi-capacity-debug-v26",
		"--set", "rbac.clusterRole.enabled=false",
		"--set", "sync.fromHost.csiStorageCapacities.enabled=true",
		"--set", "sync.fromHost.storageClasses.enabled=true",
		"--set", "sync.fromHost.csiNodes.enabled=false",
		"--set", "sync.fromHost.csiDrivers.enabled=false",
	}
	if openShift {
		args = append(args, "--set-string", "controlPlane.statefulSet.security.profile=restricted")
	}
	if publicHost != "" {
		args = append(args, "--set-string", "controlPlane.proxy.extraSANs[0]="+publicHost)
	}
	return args
}

func (h HelmInstaller) run(ctx context.Context, helmPath string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, helmPath, args...)
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME=/tmp/helm/cache",
		"HELM_CONFIG_HOME=/tmp/helm/config",
		"HELM_DATA_HOME=/tmp/helm/data",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}
