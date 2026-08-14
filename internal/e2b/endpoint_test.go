package e2b

import (
	"fmt"
	"strings"
	"testing"
)

func TestEndpointHeadersAreIndependentAndFormattingRedactsTrafficToken(t *testing.T) {
	const trafficToken = "scoped-traffic-token"
	endpoint := Endpoint{
		ListenURL:          "ws://0.0.0.0:4500",
		DialURL:            "wss://4500-provider-1.e2b.app",
		trafficAccessToken: trafficToken,
	}

	mutated := endpoint.Headers()
	mutated.Set(trafficAccessHeader, "mutated")
	deleted := endpoint.Headers()
	deleted.Del(trafficAccessHeader)
	if got := endpoint.Headers().Get(trafficAccessHeader); got != trafficToken {
		t.Fatalf("fresh endpoint traffic header = %q, want original token", got)
	}

	for _, format := range []string{"%v", "%#v"} {
		if rendered := fmt.Sprintf(format, endpoint); strings.Contains(rendered, trafficToken) {
			t.Fatalf("endpoint format %q leaked its traffic token: %s", format, rendered)
		}
	}
}
