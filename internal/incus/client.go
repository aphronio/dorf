package incus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

const (
	DefaultUnixSocket  = "/var/lib/incus/unix.socket"
	DefaultProject     = "dorf"
	DefaultStoragePool = "default"
)

var ErrNotFound = errors.New("Incus resource not found")

type Instance struct {
	Name    string
	Config  map[string]string
	Running bool
}

type CreateInstanceRequest struct {
	Name        string
	Image       string
	Network     string
	StoragePool string
	DiskSize    string
	Config      map[string]string
}

// Client is Dorf's narrow Incus authority. The official Incus SDK is the only
// production implementation; the interface exists so behavior can be tested
// without a daemon.
type Client interface {
	Instances(context.Context) ([]Instance, error)
	Instance(context.Context, string) (Instance, error)
	CreateInstance(context.Context, CreateInstanceRequest) error
	PatchInstanceConfig(context.Context, string, map[string]string, map[string]string) error
	StartInstance(context.Context, string) error
	DeleteInstance(context.Context, string, map[string]string) error
	Exec(context.Context, string, []byte, ...string) (Result, error)
	NetworkIPv4(context.Context, string) (string, error)
	OpenPortForward(context.Context, string, string, int) (net.Conn, error)
	Close()
}

type ClientFactory interface {
	Open(context.Context, ConnectionConfig) (Client, error)
}

// ConnectionConfig names one explicit Incus authority. Local connections use
// an absolute unix:// socket URL. Remote connections use one HTTPS origin and
// an exact pinned server certificate plus a client certificate/key pair.
type ConnectionConfig struct {
	Endpoint             string
	Project              string
	StoragePool          string
	TLSServerCertificate string
	TLSClientCertificate string
	TLSClientKey         string
}

func DefaultConnectionConfig() ConnectionConfig {
	return ConnectionConfig{
		Endpoint:    "unix://" + DefaultUnixSocket,
		Project:     DefaultProject,
		StoragePool: DefaultStoragePool,
	}
}

func (c ConnectionConfig) Validate() error {
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		return fmt.Errorf("Incus endpoint is required; ambient CLI configuration is not accepted")
	}
	if strings.TrimSpace(c.Project) == "" {
		return fmt.Errorf("Incus project is required")
	}
	if c.Project != strings.TrimSpace(c.Project) {
		return fmt.Errorf("Incus project must not contain surrounding whitespace")
	}
	if strings.TrimSpace(c.StoragePool) == "" {
		return fmt.Errorf("Incus storage pool is required")
	}
	if c.StoragePool != strings.TrimSpace(c.StoragePool) {
		return fmt.Errorf("Incus storage pool must not contain surrounding whitespace")
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("Incus endpoint is invalid: %w", err)
	}
	switch parsed.Scheme {
	case "unix":
		if parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil || parsed.Path == "" || !filepath.IsAbs(parsed.Path) {
			return fmt.Errorf("Incus unix endpoint must name one absolute socket path")
		}
		if c.TLSServerCertificate != "" || c.TLSClientCertificate != "" || c.TLSClientKey != "" {
			return fmt.Errorf("Incus unix endpoint does not accept remote TLS identity")
		}
	case "https":
		if parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("remote Incus endpoint must be one HTTPS origin")
		}
		if strings.TrimSpace(c.TLSServerCertificate) == "" {
			return fmt.Errorf("remote Incus server certificate pin is required")
		}
		if strings.TrimSpace(c.TLSClientCertificate) == "" {
			return fmt.Errorf("remote Incus client certificate is required")
		}
		if strings.TrimSpace(c.TLSClientKey) == "" {
			return fmt.Errorf("remote Incus client key is required")
		}
		if err := validateCertificatePEM("server", c.TLSServerCertificate); err != nil {
			return err
		}
		if _, err := tls.X509KeyPair([]byte(c.TLSClientCertificate), []byte(c.TLSClientKey)); err != nil {
			return fmt.Errorf("remote Incus client certificate/key is invalid: %w", err)
		}
	default:
		return fmt.Errorf("Incus endpoint must use unix or https")
	}
	return nil
}

func validateCertificatePEM(label, value string) error {
	block, rest := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return fmt.Errorf("remote Incus %s certificate is invalid", label)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("remote Incus %s certificate is invalid: %w", label, err)
	}
	return nil
}
