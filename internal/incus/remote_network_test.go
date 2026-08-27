package incus

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
)

func TestPreparedRemoteNetworkRequiresManagedBridge(t *testing.T) {
	network := preparedRemoteNetwork()
	if err := attestPreparedRemoteNetwork(network); err != nil {
		t.Fatal(err)
	}

	for _, mutate := range []func(*api.Network){
		func(value *api.Network) { value.Name = "incusbr0" },
		func(value *api.Network) { value.Type = "physical" },
		func(value *api.Network) { value.Managed = false },
	} {
		broken := cloneRemoteNetwork(network)
		mutate(broken)
		if err := attestPreparedRemoteNetwork(broken); err == nil || !strings.Contains(err.Error(), "managed bridge") {
			t.Fatalf("network error=%v", err)
		}
	}
}

func TestPreparedRemoteNetworkAcceptsRestrictedReadOnlyView(t *testing.T) {
	network := preparedRemoteNetwork()
	network.Config = nil
	if err := attestPreparedRemoteNetwork(network); err != nil {
		t.Fatalf("restricted shared-network view: %v", err)
	}
}

func TestRemoteNetworkDeviceForcesIsolationWithoutChangingLocalDevice(t *testing.T) {
	local := map[string]string{"type": "nic", "name": "eth0", "network": "incusbr0"}
	if got := instanceNetworkDevice("incusbr0"); !reflect.DeepEqual(got, local) {
		t.Fatalf("local device=%#v", got)
	}

	remote := instanceNetworkDevice(RemoteNetworkName)
	want := map[string]string{
		"type": "nic", "name": "eth0", "network": RemoteNetworkName,
		"security.ipv4_filtering": "true", "security.ipv6_filtering": "true", "security.port_isolation": "true",
	}
	if !reflect.DeepEqual(remote, want) {
		t.Fatalf("remote device=%#v", remote)
	}
}

func preparedRemoteNetwork() *api.Network {
	return &api.Network{
		Name: RemoteNetworkName, Type: "bridge", Managed: true,
		NetworkPut: api.NetworkPut{Config: api.ConfigMap{
			"ipv4.address": "10.254.254.1/24", "ipv4.dhcp": "true", "ipv4.nat": "true", "ipv6.address": "none",
			"security.acls": RemoteNetworkACLName, "security.acls.default.ingress.action": "reject",
			"security.acls.default.egress.action": "reject",
		}},
	}
}

func cloneRemoteNetwork(network *api.Network) *api.Network {
	cloned := *network
	cloned.Config = api.ConfigMap(cloneStrings(network.Config))
	return &cloned
}
