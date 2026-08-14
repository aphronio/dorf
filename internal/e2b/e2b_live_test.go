package e2b

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestLiveLifecycleRecoversLostCreateResponse(t *testing.T) {
	if os.Getenv("DORF_E2B_LIVE") != "1" {
		t.Skip("set DORF_E2B_LIVE=1 to mutate the configured E2B account")
	}
	apiKey := os.Getenv("E2B_API_KEY")
	template := os.Getenv("DORF_E2B_TEMPLATE")
	if apiKey == "" || template == "" {
		t.Fatal("E2B_API_KEY and DORF_E2B_TEMPLATE are required")
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	owner := Ownership{
		JobID:          "e2b-lifecycle-proof-" + nonce[:12],
		SandboxID:      "dorf-e2b-proof-" + nonce[:12],
		OwnershipNonce: nonce,
	}
	transport := &liveLostResponseTransport{base: http.DefaultTransport}
	client := Client{APIKey: apiKey, HTTPClient: &http.Client{Transport: transport}}
	t.Logf("creating restricted E2B lifecycle proof for %s", owner.SandboxID)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var providerID string
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		if providerID == "" {
			owned, err := client.FindOwned(cleanupCtx, owner)
			if err == nil && owned != nil {
				providerID = owned.ProviderID
			}
		}
		if providerID != "" {
			if err := client.DeleteOwned(cleanupCtx, providerID, owner); err != nil {
				t.Errorf("cleanup E2B Sandbox %s: %v", providerID, err)
			}
		}
	}()

	_, createErr := client.Create(ctx, CreateRequest{Template: template, Timeout: 10 * time.Minute, Owner: owner})
	if createErr == nil || !errors.Is(createErr, errInjectedLostCreateResponse) {
		t.Fatalf("create error = %v, want injected response loss", createErr)
	}
	if !transport.dropped {
		t.Fatal("the E2B success response was not discarded")
	}

	owned := waitForOwned(t, ctx, client, owner, true)
	providerID = owned.ProviderID
	detail, err := client.InspectOwned(ctx, providerID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ProviderID != providerID || detail.State != "running" {
		t.Fatalf("inspected E2B Sandbox = %#v", detail)
	}
	if err := client.DeleteOwned(ctx, providerID, owner); err != nil {
		t.Fatal(err)
	}
	waitForOwned(t, ctx, client, owner, false)
	providerID = ""
	t.Logf("proved E2B lifecycle for %s: accepted create response lost, exact owner rediscovered, inspected, deleted, and confirmed absent", owner.SandboxID)
}

var errInjectedLostCreateResponse = errors.New("injected loss of accepted E2B create response")

type liveLostResponseTransport struct {
	base    http.RoundTripper
	dropped bool
}

func (t *liveLostResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	if t.dropped || request.Method != http.MethodPost || request.URL.Path != "/sandboxes" || response.StatusCode != http.StatusCreated {
		return response, nil
	}
	t.dropped = true
	_, copyErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("consume accepted E2B create response before fault injection: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close accepted E2B create response before fault injection: %w", closeErr)
	}
	return nil, errInjectedLostCreateResponse
}

func waitForOwned(t *testing.T, ctx context.Context, client Client, owner Ownership, present bool) *Sandbox {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		owned, err := client.FindOwned(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		if present && owned != nil {
			return owned
		}
		if !present && owned == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for E2B Sandbox present=%t: %v", present, ctx.Err())
		case <-ticker.C:
		}
	}
}
