package incus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

const virtualMachineImageType = "virtual-machine"

var exactImageFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ResolveImageFingerprint resolves one image alias or exact fingerprint
// against the explicitly configured Incus project and returns only the
// immutable, full fingerprint.
func ResolveImageFingerprint(ctx context.Context, config ConnectionConfig, reference string) (string, error) {
	client, err := openImageClient(ctx, config)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return client.resolveFingerprint(ctx, reference)
}

// InstallUnifiedVMArchive imports one already-verified unified VM archive,
// converges its friendly alias atomically, and verifies both postconditions.
// The caller retains ownership of archive.
func InstallUnifiedVMArchive(ctx context.Context, config ConnectionConfig, archive io.Reader, archiveName, fingerprint, alias string) error {
	client, err := openImageClient(ctx, config)
	if err != nil {
		return err
	}
	defer client.Close()
	return client.installUnifiedVMArchive(ctx, archive, archiveName, fingerprint, alias)
}

type imageClient struct {
	server incusclient.InstanceServer
}

func openImageClient(ctx context.Context, config ConnectionConfig) (*imageClient, error) {
	server, err := (SDKClientFactory{}).openServer(ctx, config)
	if err != nil {
		return nil, err
	}
	return &imageClient{server: server}, nil
}

func (c *imageClient) Close() { c.server.Disconnect() }

func (c *imageClient) serverFor(ctx context.Context) incusclient.InstanceServer {
	if contextual, ok := c.server.(interface {
		WithContext(context.Context) incusclient.InstanceServer
	}); ok {
		return contextual.WithContext(ctx)
	}
	return c.server
}

func (c *imageClient) resolveFingerprint(ctx context.Context, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", fmt.Errorf("Incus image reference is required")
	}
	server := c.serverFor(ctx)
	if exactImageFingerprint.MatchString(reference) {
		return exactFingerprintFromImage(server, reference)
	}
	alias, _, err := server.GetImageAlias(reference)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return "", fmt.Errorf("Incus image reference %q was not found", reference)
	}
	if err != nil {
		return "", fmt.Errorf("resolve Incus image alias %q: %w", reference, err)
	}
	if alias.Type != "" && alias.Type != virtualMachineImageType {
		return "", fmt.Errorf("Incus image alias %q is not a virtual-machine image", reference)
	}
	if !exactImageFingerprint.MatchString(alias.Target) {
		return "", fmt.Errorf("Incus image alias %q has no exact fingerprint target", reference)
	}
	return exactFingerprintFromImage(server, alias.Target)
}

func exactFingerprintFromImage(server incusclient.InstanceServer, reference string) (string, error) {
	image, _, err := server.GetImage(reference)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return "", fmt.Errorf("Incus image %q was not found", reference)
	}
	if err != nil {
		return "", fmt.Errorf("inspect Incus image %q: %w", reference, err)
	}
	fingerprint := strings.ToLower(strings.TrimSpace(image.Fingerprint))
	if !exactImageFingerprint.MatchString(fingerprint) {
		return "", fmt.Errorf("Incus image reference %q did not resolve to an exact fingerprint", reference)
	}
	if exactImageFingerprint.MatchString(reference) && fingerprint != strings.ToLower(reference) {
		return "", fmt.Errorf("Incus image reference %q resolved to a different fingerprint", reference)
	}
	if image.Type != virtualMachineImageType {
		return "", fmt.Errorf("Incus image %s is not a virtual-machine image", fingerprint)
	}
	return fingerprint, nil
}

func (c *imageClient) installUnifiedVMArchive(ctx context.Context, archive io.Reader, archiveName, fingerprint, aliasName string) error {
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	aliasName = strings.TrimSpace(aliasName)
	if archive == nil {
		return fmt.Errorf("verified Incus image archive is required")
	}
	if !exactImageFingerprint.MatchString(fingerprint) {
		return fmt.Errorf("exact Incus image fingerprint is required")
	}
	if aliasName == "" {
		return fmt.Errorf("Incus image alias is required")
	}
	server := c.serverFor(ctx)
	installed, _, err := server.GetImage(fingerprint)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		op, createErr := server.CreateImage(api.ImagesPost{Filename: archiveName}, &incusclient.ImageCreateArgs{
			MetaFile: archive,
			MetaName: archiveName,
			Type:     virtualMachineImageType,
		})
		if createErr != nil {
			return fmt.Errorf("import verified Incus VM image %s: %w", fingerprint, createErr)
		}
		if waitErr := op.WaitContext(ctx); waitErr != nil {
			return fmt.Errorf("import verified Incus VM image %s: %w", fingerprint, waitErr)
		}
	} else if err != nil {
		return fmt.Errorf("inspect Incus image %s before import: %w", fingerprint, err)
	} else if installed.Fingerprint != fingerprint || installed.Type != virtualMachineImageType {
		return fmt.Errorf("Incus image %s does not have the verified VM identity", fingerprint)
	}

	if _, err := exactFingerprintFromImage(server, fingerprint); err != nil {
		return fmt.Errorf("verify imported Incus image: %w", err)
	}
	alias, etag, err := server.GetImageAlias(aliasName)
	switch {
	case api.StatusErrorCheck(err, http.StatusNotFound):
		err = server.CreateImageAlias(api.ImageAliasesPost{ImageAliasesEntry: api.ImageAliasesEntry{
			Name:                 aliasName,
			Type:                 virtualMachineImageType,
			ImageAliasesEntryPut: api.ImageAliasesEntryPut{Target: fingerprint},
		}})
		if err != nil {
			return fmt.Errorf("create Incus image alias %s: %w", aliasName, err)
		}
	case err != nil:
		return fmt.Errorf("inspect Incus image alias %s: %w", aliasName, err)
	case alias.Type != "" && alias.Type != virtualMachineImageType:
		return fmt.Errorf("Incus image alias %s is not a virtual-machine image", aliasName)
	case alias.Target != fingerprint:
		err = server.UpdateImageAlias(aliasName, api.ImageAliasesEntryPut{Description: alias.Description, Target: fingerprint}, etag)
		if err != nil {
			return fmt.Errorf("update Incus image alias %s: %w", aliasName, err)
		}
	}

	resolved, err := c.resolveFingerprint(ctx, aliasName)
	if err != nil {
		return fmt.Errorf("verify installed Incus image alias %s: %w", aliasName, err)
	}
	if resolved != fingerprint {
		return fmt.Errorf("installed Incus image alias %s does not resolve to the verified fingerprint", aliasName)
	}
	return nil
}
