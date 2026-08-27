package incus

import (
	"fmt"

	"github.com/lxc/incus/v7/shared/api"
)

const (
	RemoteProjectName    = "dorf-remote"
	RemoteNetworkName    = "dorfbr0"
	RemoteNetworkACLName = "dorf-egress"
)

func attestPreparedEnrollmentProject(project *api.Project) error {
	if project == nil || project.Name != RemoteProjectName {
		return fmt.Errorf("required Incus project %s is missing", RemoteProjectName)
	}
	required := map[string]string{
		"restricted":                      "true",
		"features.images":                 "true",
		"features.networks":               "false",
		"features.profiles":               "true",
		"features.storage.buckets":        "false",
		"features.storage.volumes":        "true",
		"limits.instances":                "4",
		"limits.virtual-machines":         "4",
		"restricted.networks.access":      RemoteNetworkName,
		"restricted.storage-pools.access": DefaultStoragePool,
	}
	for key, value := range required {
		if project.Config[key] != value {
			return fmt.Errorf("Incus project %s requires %s=%s", RemoteProjectName, key, value)
		}
	}
	return nil
}

func attestPreparedRemoteNetwork(network *api.Network) error {
	if network == nil || network.Name != RemoteNetworkName || network.Type != "bridge" || !network.Managed {
		return fmt.Errorf("required Incus network %s is not a managed bridge", RemoteNetworkName)
	}
	return nil
}
