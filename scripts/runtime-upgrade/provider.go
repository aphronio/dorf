// This disposable proof bridge exercises Dorf's shipped provider adapters.
// It reads ownership through stdin and never prints ownership tokens or keys.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/incus"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
)

type request struct {
	Provider    string              `json:"provider"`
	Operation   string              `json:"operation"`
	Key         string              `json:"key"`
	Owner       provider.Ownership  `json:"owner"`
	Destination provider.Ownership  `json:"destination"`
	Checkpoint  provider.Checkpoint `json:"checkpoint"`
	Project     string              `json:"project"`
	EventsFile  string              `json:"events_file"`
}

func run() error {
	var input request
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		return err
	}
	if input.Operation == "publish" {
		return publish(input.EventsFile)
	}
	var checkpoints provider.Checkpointer
	switch input.Provider {
	case "incus":
		connection := incus.DefaultConnectionConfig()
		connection.Project = input.Project
		checkpoints = incus.Sandbox{Config: incus.Config{Connection: connection}}
	case "e2b":
		checkpoints = e2b.Adapter{Client: e2b.Client{APIKey: os.Getenv("E2B_API_KEY")},
			Config: e2b.AdapterConfig{SandboxTimeout: time.Hour, AllowInternet: true}}
	default:
		return fmt.Errorf("unknown proof provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var result any
	switch input.Operation {
	case "capture":
		checkpoint, err := checkpoints.CaptureCheckpoint(ctx, input.Owner, input.Key)
		if err != nil {
			return err
		}
		result = checkpoint
	case "restore":
		id, err := checkpoints.RestoreCheckpoint(ctx, input.Owner, input.Destination, input.Checkpoint)
		if err != nil {
			return err
		}
		result = map[string]string{"provider_id": id}
	case "delete":
		if err := checkpoints.DeleteCheckpoint(ctx, input.Owner, input.Checkpoint); err != nil {
			return err
		}
		result = map[string]bool{"deleted": true}
	default:
		return fmt.Errorf("unknown proof operation")
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func publish(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	publisher, err := telemetry.FromEnv(ctx)
	if err != nil {
		return err
	}
	if publisher == nil {
		return fmt.Errorf("OTLP logging is not configured")
	}
	defer publisher.Shutdown(ctx)
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	count := 0
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		event, err := proofEvent(lines.Bytes())
		if err != nil {
			return err
		}
		publisher.Emit(event)
		count++
	}
	if err := lines.Err(); err != nil {
		return err
	}
	if err := publisher.Shutdown(ctx); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]int{"events_exported": count})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func proofEvent(data []byte) (telemetry.Event, error) {
	var native telemetry.Event
	if err := json.Unmarshal(data, &native); err != nil {
		return telemetry.Event{}, err
	}
	if native.Name != "" {
		if !strings.HasPrefix(native.Name, "dorf.upgrade.") || native.Attributes["dorf.synthetic_proof"] != true {
			return telemetry.Event{}, fmt.Errorf("expected a synthetic upgrade event")
		}
		return native, nil
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		return telemetry.Event{}, err
	}
	phase, _ := event["phase"].(string)
	stamp, _ := event["at"].(string)
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return telemetry.Event{}, err
	}
	event["dorf.upgrade_id"] = event["upgrade_id"]
	event["dorf.session_id"] = event["upgrade_id"]
	event["dorf.sandbox_id"] = event["upgrade_id"]
	event["dorf.provider_sandbox_id"] = event["provider_sandbox_id"]
	event["dorf.synthetic_proof"] = true
	return telemetry.Event{Name: "dorf.upgrade." + phase, At: at, Attributes: event, Failed: strings.HasSuffix(phase, ".failed")}, nil
}
