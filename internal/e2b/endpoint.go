package e2b

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const trafficAccessHeader = "e2b-traffic-access-token"

// Endpoint is one authenticated host-to-Sandbox HTTP/WebSocket endpoint. The
// provider traffic capability remains private and is exposed only as a cloned
// request header set at dial time.
type Endpoint struct {
	ListenURL          string
	DialURL            string
	trafficAccessToken string
}

func (e Endpoint) String() string {
	return fmt.Sprintf("E2B endpoint %s -> %s (scoped traffic token redacted)", e.ListenURL, e.DialURL)
}

func (e Endpoint) GoString() string { return e.String() }

func (e Endpoint) Headers() http.Header {
	headers := make(http.Header)
	if e.trafficAccessToken != "" {
		headers.Set(trafficAccessHeader, e.trafficAccessToken)
	}
	return headers
}

// ConnectEndpoint extends the Sandbox lifetime and resolves E2B's TLS port
// proxy. The process binds inside the VM while Dorf dials the provider URL.
func (c Client) ConnectEndpoint(ctx context.Context, providerID string, port int, timeout time.Duration) (Endpoint, error) {
	if strings.TrimSpace(providerID) == "" {
		return Endpoint{}, fmt.Errorf("E2B provider Sandbox ID is required")
	}
	if port < 1 || port > 65535 {
		return Endpoint{}, fmt.Errorf("E2B endpoint port must be between 1 and 65535")
	}
	response, err := c.connect(ctx, providerID, timeout)
	if err != nil {
		return Endpoint{}, err
	}
	if response.TrafficAccessToken == "" {
		return Endpoint{}, fmt.Errorf("E2B connect response omitted its scoped traffic token")
	}
	domain := strings.TrimSpace(response.Domain)
	if domain == "" {
		domain = "e2b.app"
	}
	if strings.ContainsAny(domain, "/:") {
		return Endpoint{}, fmt.Errorf("E2B Sandbox domain %q is invalid", domain)
	}
	dial := (&url.URL{
		Scheme: "wss",
		Host:   strconv.Itoa(port) + "-" + providerID + "." + domain,
	}).String()
	return Endpoint{
		ListenURL:          "ws://0.0.0.0:" + strconv.Itoa(port),
		DialURL:            dial,
		trafficAccessToken: response.TrafficAccessToken,
	}, nil
}
