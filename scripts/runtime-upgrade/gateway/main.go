// Disposable live-proof route authority. Run on the configured Gateway host.
// Stdout carries a scoped key to the test through SSH; never render or log it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/gateway"
)

type request struct {
	Operation string `json:"operation"`
	SandboxID string `json:"sandbox_id"`
	RouteID   string `json:"route_id"`
	URL       string `json:"url"`
	Model     string `json:"model"`
}

func run() error {
	var r request
	if err := json.NewDecoder(os.Stdin).Decode(&r); err != nil {
		return err
	}
	if !regexp.MustCompile(`^dorf-[a-f0-9]{20}$`).MatchString(r.SandboxID) || !regexp.MustCompile(`^route-[a-f0-9]{16}$`).MatchString(r.RouteID) {
		return fmt.Errorf("invalid proof route identity")
	}
	paths, err := config.CurrentOperatorPaths()
	if err != nil {
		return err
	}
	g := gateway.Gateway{StatePath: filepath.Join(paths.DataDir, "provider-gateway"), InternalDialOrigin: os.Getenv("DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	consumer := "upgrade-proof:sandbox:" + r.SandboxID
	if r.Operation == "revoke" {
		if err := g.RevokeExact(ctx, consumer, r.RouteID); err != nil {
			return err
		}
		_, present, err := g.Route(ctx, consumer)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("proof route remains after revocation")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]bool{"revoked": true})
	}
	if r.Operation != "create" {
		return fmt.Errorf("unknown proof operation")
	}
	if err := g.CheckRemote(ctx, r.URL); err != nil {
		return err
	}
	connection, err := g.DefaultConnection()
	if err != nil {
		return err
	}
	route, err := g.ReconcileCreate(ctx, connection, consumer, r.RouteID)
	if err != nil {
		return err
	}
	if err := g.RequireModel(ctx, r.URL, route.APIKey, r.Model); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"key": route.APIKey})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
