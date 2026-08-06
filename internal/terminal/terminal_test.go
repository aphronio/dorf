package terminal

import "testing"

func TestSandboxRoutesRequireExactConfiguredBridgeAddress(t *testing.T) {
	if err := requireBridgeRoute("http://10.42.0.1:8317/v1", "10.42.0.1"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"http://10.43.0.1:8317/v1", "http://127.0.0.1:8317/v1", "http://0.0.0.0:8317/v1", "http://192.0.2.10:8317/v1", "https://10.42.0.1:8317/v1"} {
		if err := requireBridgeRoute(value, "10.42.0.1"); err == nil {
			t.Fatalf("accepted unsafe Sandbox route %s", value)
		}
	}
}
