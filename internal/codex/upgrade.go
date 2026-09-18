package codex

import (
	"context"
	"fmt"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type retainedTurn struct {
	ThreadID    string `json:"thread_id"`
	TurnID      string `json:"turn_id"`
	TurnOutcome string `json:"status"`
}

// VerifyRetainedThread checks the continuity manifest captured with the backup.
func (a Agent) VerifyRetainedThread(ctx context.Context, owner provider.Ownership, threadID string) error {
	return a.verifyContinuity(ctx, owner, threadID, false)
}
func (a Agent) VerifyUpgrade(ctx context.Context, owner provider.Ownership, threadID string) error {
	return a.verifyContinuity(ctx, owner, threadID, true)
}
func (a Agent) verifyContinuity(ctx context.Context, owner provider.Ownership, threadID string, resume bool) error {
	if threadID == "" {
		return nil
	}
	expected, err := a.readContinuity(ctx, owner, threadID)
	if err != nil {
		return err
	}
	return a.withServer(ctx, owner, func(p *protocol) error {
		if resume {
			if err := p.resumeThread(ctx, threadID); err != nil {
				return err
			}
		}
		turns, err := p.readTurns(ctx, threadID)
		if err != nil {
			return err
		}
		return verifyRetainedTurns(expected, turns)
	})
}
func verifyRetainedTurns(expected []retainedTurn, turns []TurnOutcome) error {
	observed := make(map[string]string, len(turns))
	for _, turn := range turns {
		if !turn.Terminal() {
			return fmt.Errorf("native Thread is not quiescent")
		}
		observed[turn.ID] = turn.Status
	}
	for _, turn := range expected {
		if observed[turn.TurnID] != turn.TurnOutcome {
			return fmt.Errorf("native verification did not retain exact settled Turn history")
		}
	}
	return nil
}

// Quiesce stops only the authenticated process already owned by Dorf.
// An absent server is already quiescent; this never starts or resumes one.
// Unknown or unauthenticated app-server processes are attention, not kill targets.
func (a Agent) Quiesce(ctx context.Context, owner provider.Ownership, threadID string) error {
	return a.withSandboxAccess(ctx, owner, func(a Agent) error {
		endpoint, err := a.Sandbox.Endpoint(ctx, owner, a.Port)
		if err != nil {
			return err
		}
		probe, err := a.probeServer(ctx, owner, endpoint.ListenURL)
		if err != nil {
			return err
		}
		if !probe.running {
			if threadID == "" {
				return nil
			}
			_, err := a.readContinuity(ctx, owner, threadID)
			return err
		}
		if !probe.tracked || probe.token == "" {
			return fmt.Errorf("cannot stop an unproven app-server")
		}
		p, err := dialProtocol(ctx, endpoint.DialURL, probe.token, endpoint.Headers(), endpoint.DialContext())
		if err != nil {
			return err
		}
		p.configureObservations(ctx, a.Observations, owner)
		defer p.finish()
		if _, err := a.captureContinuity(ctx, owner, p, threadID); err != nil {
			return err
		}
		// Recheck the token digest and exact tracked command in the guest before
		// signalling. Avoid PID reuse by opening a pidfd before inspecting /proc.
		result, err := a.Sandbox.Exec(ctx, owner, []byte(probe.token), "python3", "-c", stopUpgradeServer, endpoint.ListenURL)
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("tracked app-server did not quiesce (exit %d)", result.ExitCode)
		}
		return nil
	})
}

const stopUpgradeServer = `import ctypes,errno,hashlib,os,select,signal,sys
from pathlib import Path
root=Path('/tmp/dorf')
pidfile=root/'codex-app-server.pid'
if not pidfile.exists(): sys.exit(0)
pid=int(pidfile.read_text().strip())
# The supported guest images may ship Python without pidfd wrappers. Both
# supported Linux architectures use these generic syscall numbers.
libc=ctypes.CDLL(None,use_errno=True)
handle=libc.syscall(434,pid,0) # pidfd_open
if handle<0:
    if ctypes.get_errno()==errno.ESRCH: sys.exit(0)
    sys.exit(4)
args=Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')
digest=hashlib.sha256(sys.stdin.buffer.read()).hexdigest().encode()
def pair(key,value):
    return any(args[i:i+2]==[key,value] for i in range(len(args)-1))
if b'app-server' not in args or not pair(b'--listen',sys.argv[1].encode()) or not pair(b'--ws-token-sha256',digest):
    sys.exit(2)
if libc.syscall(424,handle,signal.SIGTERM,None,0)<0: sys.exit(5) # pidfd_send_signal
if not select.select([handle],[],[],15)[0]: sys.exit(3)
pidfile.unlink(missing_ok=True)
`
