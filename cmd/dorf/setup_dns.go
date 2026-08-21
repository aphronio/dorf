package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type dnsResolver interface {
	LookupNS(context.Context, string) ([]*net.NS, error)
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type dnsOverHTTPSResolver struct {
	Client   *http.Client
	Endpoint string
}

type dnsOverHTTPSResponse struct {
	Status int `json:"Status"`
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

type dnsDelegation struct {
	Zone        string
	Nameservers []string
	Cloudflare  bool
}

func discoverDNSDelegation(ctx context.Context, resolver dnsResolver, hostname string) (dnsDelegation, error) {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), "."), ".")
	for offset := 0; offset < len(labels)-1; offset++ {
		zone := strings.Join(labels[offset:], ".")
		records, err := resolver.LookupNS(ctx, zone)
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				continue
			}
			return dnsDelegation{}, err
		}
		if len(records) == 0 {
			continue
		}

		nameservers := make([]string, 0, len(records))
		cloudflare := true
		for _, record := range records {
			name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(record.Host)), ".")
			if name == "" {
				continue
			}
			nameservers = append(nameservers, name)
			if !strings.HasSuffix(name, ".ns.cloudflare.com") {
				cloudflare = false
			}
		}
		if len(nameservers) == 0 {
			continue
		}
		sort.Strings(nameservers)
		return dnsDelegation{Zone: zone, Nameservers: nameservers, Cloudflare: cloudflare}, nil
	}
	return dnsDelegation{}, nil
}

func hostnameHasAddresses(ctx context.Context, resolver dnsResolver, hostname string) (bool, error) {
	addresses, err := resolver.LookupIPAddr(ctx, strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return false, nil
		}
		return false, err
	}
	return len(addresses) > 0, nil
}

func freshDNSHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	resolver := dnsOverHTTPSResolver{
		Client:   &http.Client{Timeout: 5 * time.Second},
		Endpoint: "https://cloudflare-dns.com/dns-query",
	}
	return resolvedHTTPClient(resolver, dialer.DialContext)
}

func (resolver dnsOverHTTPSResolver) LookupIPAddr(ctx context.Context, hostname string) ([]net.IPAddr, error) {
	client := resolver.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	endpoint := strings.TrimSpace(resolver.Endpoint)
	if endpoint == "" {
		endpoint = "https://cloudflare-dns.com/dns-query"
	}
	var addresses []net.IPAddr
	for _, recordType := range []string{"A", "AAAA"} {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		query := parsed.Query()
		query.Set("name", hostname)
		query.Set("type", recordType)
		parsed.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/dns-json")
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		closeErr := response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("DNS-over-HTTPS returned HTTP %d", response.StatusCode)
		}
		var decoded dnsOverHTTPSResponse
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("decode DNS-over-HTTPS response: %w", err)
		}
		if decoded.Status != 0 {
			return nil, fmt.Errorf("DNS-over-HTTPS returned status %d", decoded.Status)
		}
		for _, answer := range decoded.Answer {
			if answer.Type != 1 && answer.Type != 28 {
				continue
			}
			if parsedIP := net.ParseIP(answer.Data); parsedIP != nil {
				addresses = append(addresses, net.IPAddr{IP: parsedIP})
			}
		}
	}
	return addresses, nil
}

func resolvedHTTPClient(resolver ipResolver, dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var failures []error
		for _, resolved := range addresses {
			connection, dialErr := dial(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			failures = append(failures, dialErr)
		}
		if len(failures) == 0 {
			return nil, fmt.Errorf("DNS returned no addresses for %s", host)
		}
		return nil, errors.Join(failures...)
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: transport}
}
