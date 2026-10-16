package platform

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const openShiftSecurityAPIGroup = "security.openshift.io"

type Platform string

const (
	PlatformOpenShift Platform = "openshift"
	PlatformCoreWeave Platform = "coreweave"
)

type groupDiscovery interface {
	ServerGroups() (*metav1.APIGroupList, error)
}

// IsOpenShift identifies OpenShift through API discovery. Discovery failures are
// returned rather than silently selecting a potentially incompatible profile.
func IsOpenShift(_ context.Context, discovery groupDiscovery) (bool, error) {
	groups, err := discovery.ServerGroups()
	if err != nil {
		return false, err
	}
	for _, group := range groups.Groups {
		if group.Name == openShiftSecurityAPIGroup {
			return true, nil
		}
	}
	return false, nil
}

// Resolve selects an explicitly configured platform, or uses API discovery to
// select OpenShift when PLATFORM is unset. Other platforms must be configured.
func Resolve(configured string, openShiftDetected bool) (Platform, error) {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "":
		if openShiftDetected {
			return PlatformOpenShift, nil
		}
		return "", nil
	case string(PlatformOpenShift):
		if !openShiftDetected {
			return "", fmt.Errorf("PLATFORM=openshift configured but OpenShift APIs were not detected")
		}
		return PlatformOpenShift, nil
	case string(PlatformCoreWeave):
		if openShiftDetected {
			return "", fmt.Errorf("PLATFORM=coreweave configured but OpenShift APIs were detected")
		}
		return PlatformCoreWeave, nil
	default:
		return "", fmt.Errorf("unsupported PLATFORM %q; expected openshift or coreweave", configured)
	}
}
