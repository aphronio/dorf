package gateway

import (
	"net"
	"strings"
	"testing"
)

func TestProviderBindIsLoopbackOrExactPrivateIncusBridge(t *testing.T) {
	bridge := []net.Addr{&net.IPNet{IP: net.ParseIP("10.44.0.1"), Mask: net.CIDRMask(24, 32)}}
	tests := []struct {
		name       string
		bind       string
		addresses  []net.Addr
		want       string
		wantRemote bool
		wantError  string
	}{
		{name: "loopback", bind: "127.0.0.2", want: "127.0.0.2"},
		{name: "private Incus bridge", bind: "10.44.0.1", addresses: bridge, want: "10.44.0.1", wantRemote: true},
		{name: "private LAN", bind: "192.168.1.20", addresses: bridge, wantError: "not assigned"},
		{name: "public interface", bind: "203.0.113.10", addresses: bridge, wantError: "public interface"},
		{name: "wildcard", bind: "0.0.0.0", wantError: "loopback or"},
		{name: "link local", bind: "169.254.1.1", wantError: "loopback or"},
		{name: "IPv6", bind: "::1", wantError: "loopback or"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, remote, err := validateProviderBind(test.bind, test.addresses)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error=%v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil || got != test.want || remote != test.wantRemote {
				t.Fatalf("bind=%q remote=%t error=%v", got, remote, err)
			}
		})
	}
}
