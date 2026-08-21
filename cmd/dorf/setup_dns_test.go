package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type fakeDNSResolver struct {
	records   map[string][]*net.NS
	addresses map[string][]net.IPAddr
	errors    map[string]error
}

type fixedIPResolver struct {
	addresses []net.IPAddr
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func (resolver fixedIPResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return resolver.addresses, nil
}

func (resolver fakeDNSResolver) LookupNS(_ context.Context, zone string) ([]*net.NS, error) {
	if err := resolver.errors[zone]; err != nil {
		return nil, err
	}
	if records, ok := resolver.records[zone]; ok {
		return records, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: zone, IsNotFound: true}
}

func (resolver fakeDNSResolver) LookupIPAddr(_ context.Context, hostname string) ([]net.IPAddr, error) {
	if err := resolver.errors[hostname+" A/AAAA"]; err != nil {
		return nil, err
	}
	if addresses, ok := resolver.addresses[hostname]; ok {
		return addresses, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: hostname, IsNotFound: true}
}

func TestDiscoverDNSDelegationFindsTheNearestAuthoritativeZone(t *testing.T) {
	resolver := fakeDNSResolver{records: map[string][]*net.NS{
		"example.org": {
			{Host: "lana.ns.cloudflare.com."},
			{Host: "cash.ns.cloudflare.com."},
		},
	}}
	got, err := discoverDNSDelegation(context.Background(), resolver, "DORF.Example.ORG.")
	if err != nil {
		t.Fatal(err)
	}
	want := dnsDelegation{
		Zone:        "example.org",
		Nameservers: []string{"cash.ns.cloudflare.com", "lana.ns.cloudflare.com"},
		Cloudflare:  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delegation=%#v want %#v", got, want)
	}
}

func TestDiscoverDNSDelegationDoesNotMisclassifyOtherProviders(t *testing.T) {
	resolver := fakeDNSResolver{records: map[string][]*net.NS{
		"example.com": {
			{Host: "cash.ns.cloudflare.com."},
			{Host: "ns1.other-provider.example."},
		},
	}}
	got, err := discoverDNSDelegation(context.Background(), resolver, "dorf.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Zone != "example.com" || got.Cloudflare {
		t.Fatalf("delegation=%#v", got)
	}
}

func TestRequireCloudflareDNSReportsLookupAndProviderFailures(t *testing.T) {
	lookupErr := errors.New("resolver unavailable")
	if _, err := requireCloudflareDNS(context.Background(), fakeDNSResolver{errors: map[string]error{
		"dorf.example.com": lookupErr,
	}}, "dorf.example.com"); !errors.Is(err, lookupErr) {
		t.Fatalf("lookup error=%v", err)
	}
	if _, err := requireCloudflareDNS(context.Background(), fakeDNSResolver{records: map[string][]*net.NS{
		"example.com": {{Host: "ns1.other-provider.example."}},
	}}, "dorf.example.com"); err == nil {
		t.Fatal("non-Cloudflare delegation was accepted")
	}
}

func TestHostnameHasAddressesTreatsMissingRecordsAsAvailable(t *testing.T) {
	available, err := hostnameHasAddresses(context.Background(), fakeDNSResolver{}, "dorf.example.com")
	if err != nil || available {
		t.Fatalf("available=%v err=%v", available, err)
	}
	occupied, err := hostnameHasAddresses(context.Background(), fakeDNSResolver{addresses: map[string][]net.IPAddr{
		"dorf.example.com": {{IP: net.ParseIP("192.0.2.1")}},
	}}, "dorf.example.com")
	if err != nil || !occupied {
		t.Fatalf("occupied=%v err=%v", occupied, err)
	}
}

func TestResolvedHTTPClientBypassesAStaleSystemResolver(t *testing.T) {
	dialed := make(chan string, 1)
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		client, server := net.Pipe()
		dialed <- address
		go func() {
			defer server.Close()
			_, _ = io.WriteString(server, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		}()
		return client, nil
	}
	client := resolvedHTTPClient(fixedIPResolver{addresses: []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}}}, dial)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://newly-created.example:8317/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if got := <-dialed; got != "192.0.2.10:8317" {
		t.Fatalf("dialed=%q", got)
	}
}

func TestDNSOverHTTPSResolverReturnsOnlyAddressRecords(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"Status":0,"Answer":[{"type":5,"data":"tunnel.example"}]}`
		switch request.URL.Query().Get("type") {
		case "A":
			body = `{"Status":0,"Answer":[{"type":1,"data":"192.0.2.10"}]}`
		case "AAAA":
			body = `{"Status":0,"Answer":[{"type":28,"data":"2001:db8::10"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	addresses, err := (dnsOverHTTPSResolver{Client: client, Endpoint: "https://dns.example/query"}).LookupIPAddr(context.Background(), "dorf.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 2 || addresses[0].IP.String() != "192.0.2.10" || addresses[1].IP.String() != "2001:db8::10" {
		t.Fatalf("addresses=%v", addresses)
	}
}
