package terminal

import "testing"

func TestSandboxRoutesRequirePrivateBridgeAddress(t *testing.T) {
	if err := requirePrivateRoute("http://10.42.0.1:8317/v1"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"http://127.0.0.1:8317/v1", "http://0.0.0.0:8317/v1", "http://192.0.2.10:8317/v1", "https://10.42.0.1:8317/v1"} {
		if err := requirePrivateRoute(value); err == nil {
			t.Fatalf("accepted unsafe Sandbox route %s", value)
		}
	}
}
