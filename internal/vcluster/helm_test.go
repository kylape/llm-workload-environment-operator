package vcluster

import "testing"

func TestReleaseDeployed(t *testing.T) {
	t.Parallel()
	if !releaseDeployed([]helmRelease{{Name: "test", Status: "deployed"}}) {
		t.Fatal("deployed release was not recognized")
	}
	for _, status := range []string{"failed", "pending-install", "pending-upgrade"} {
		if releaseDeployed([]helmRelease{{Name: "test", Status: status}}) {
			t.Fatalf("%s release was treated as deployed", status)
		}
	}
}

func TestInstallArgsSelectRestrictedProfileOnlyForOpenShift(t *testing.T) {
	t.Parallel()
	const profile = "controlPlane.statefulSet.security.profile=restricted"

	openShiftArgs := installArgs("test", "env-test", "chart.tgz", true, "")
	if !contains(openShiftArgs, profile) {
		t.Fatalf("OpenShift install args do not select restricted profile: %v", openShiftArgs)
	}

	kubernetesArgs := installArgs("test", "env-test", "chart.tgz", false, "")
	if contains(kubernetesArgs, profile) {
		t.Fatalf("Kubernetes install args unexpectedly select restricted profile: %v", kubernetesArgs)
	}
}

func TestInstallArgsLimitHostStorageSync(t *testing.T) {
	t.Parallel()
	args := installArgs("test", "env-test", "chart.tgz", false, "")
	for _, value := range []string{
		"rbac.clusterRole.enabled=false",
		"sync.fromHost.csiStorageCapacities.enabled=true",
		"sync.fromHost.storageClasses.enabled=true",
		"sync.fromHost.csiNodes.enabled=false",
		"sync.fromHost.csiDrivers.enabled=false",
	} {
		if !contains(args, value) {
			t.Fatalf("install args do not contain %q: %v", value, args)
		}
	}
}

func TestInstallArgsAddPublicHostSAN(t *testing.T) {
	args := installArgs("test", "env-test", "chart.tgz", true, "test.apps.example.com")
	if !contains(args, "controlPlane.proxy.extraSANs[0]=test.apps.example.com") {
		t.Fatalf("install args do not configure public host SAN: %v", args)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
