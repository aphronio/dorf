package e2b

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestLiveMemoryPausePreservesProcessAcrossTwoResumes(t *testing.T) {
	if os.Getenv("DORF_E2B_PAUSE_LIVE") != "1" {
		t.Skip("set DORF_E2B_PAUSE_LIVE=1 for a disposable E2B lifecycle proof")
	}
	key, template := os.Getenv("E2B_API_KEY"), os.Getenv("DORF_E2B_TEMPLATE")
	if key == "" || template == "" {
		t.Fatal("E2B_API_KEY and DORF_E2B_TEMPLATE required")
	}
	client := Client{APIKey: key}
	owner := liveOwnership(t, "idle-pause")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// Cleanup discovers by exact ownership even if the create response is lost.
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		owned, err := client.FindOwned(clean, owner)
		if err == nil && owned != nil {
			err = client.DeleteOwned(clean, owned.ProviderID, owner)
		}
		if err != nil {
			t.Errorf("cleanup: %v", err)
			return
		}
		owned, err = client.FindOwned(clean, owner)
		if err != nil || owned != nil {
			t.Errorf("cleanup not verified: %v", err)
		} else {
			t.Log("exact owned resource absent after cleanup")
		}
	}()
	sandbox, err := client.Create(ctx, CreateRequest{Template: template, Timeout: 10 * time.Minute, Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	adapter := Adapter{Client: client, Config: AdapterConfig{Workspace: "/workspace", SandboxTimeout: 10 * time.Minute, ProcessTimeout: 30 * time.Second}}
	owned := provider.Ownership{SessionID: owner.SessionID, SandboxID: owner.SandboxID, OwnershipNonce: owner.OwnershipNonce}
	// An in-memory random nonce is exposed over a Unix socket. It is never saved
	// to a file, so a restarted process cannot produce the same proof.
	script := `import os,socket,uuid,json
s=socket.socket(socket.AF_UNIX);s.bind('/tmp/dorf-pause.sock');s.listen()
value=json.dumps({'pid':os.getpid(),'nonce':str(uuid.uuid4()),'boot':open('/proc/sys/kernel/random/boot_id').read().strip()}).encode()
while True:
 c,_=s.accept();c.sendall(value);c.close()
`
	if err := adapter.PutFile(ctx, owned, "/tmp/dorf-pause-proof.py", []byte(script)); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Exec(ctx, owned, nil, "sh", "-c", "nohup python3 /tmp/dorf-pause-proof.py </dev/null >/tmp/dorf-pause-proof.log 2>&1 &")
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("start background proof: %v %s", err, result.Stderr)
	}
	read := func() string {
		t.Helper()
		result, err := adapter.Exec(ctx, owned, nil, "python3", "-c", `import socket,time
for i in range(100):
 try:
  s=socket.socket(socket.AF_UNIX);s.connect('/tmp/dorf-pause.sock');print(s.recv(4096).decode());break
 except (FileNotFoundError,ConnectionRefusedError):time.sleep(.05)
else:raise RuntimeError('background process unavailable')`)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("read process: %v %s", err, result.Stderr)
		}
		return strings.TrimSpace(result.Stdout)
	}
	before := read()
	for round := 1; round <= 2; round++ {
		if err := adapter.PauseOwned(ctx, owned); err != nil {
			t.Fatal(err)
		}
		paused, err := client.InspectOwned(ctx, sandbox.ProviderID, owner)
		if err != nil || paused.State != "paused" {
			t.Fatalf("pause state=%s %v", paused.State, err)
		}
		// Passive status reads must leave the in-memory process paused.
		for range 3 {
			observed, err := adapter.ObserveOwned(ctx, owned)
			if err != nil || observed.State != "paused" || observed.Provider != "e2b" {
				t.Fatalf("passive observation=%+v %v", observed, err)
			}
		}
		stillPaused, err := client.InspectOwned(ctx, sandbox.ProviderID, owner)
		if err != nil || stillPaused.State != "paused" {
			t.Fatalf("observation woke VM: %v", err)
		}
		// Repeated idle reconciliation must not wake or re-pause the resource.
		if err := adapter.PauseOwned(ctx, owned); err != nil {
			t.Fatal(err)
		}
		if after := read(); after != before {
			t.Fatalf("process restarted across resume: before=%s after=%s", before, after)
		}
		t.Logf("round %d: paused state confirmed; adapter Exec resumed the same live process, memory nonce and boot", round)
	}
}
