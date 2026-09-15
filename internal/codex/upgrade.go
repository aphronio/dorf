package codex

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

// VerifyRetainedThreads reads the exact settled Turns without resuming a
// Thread. It never asks the model to produce a new Turn or execute tools.
func (a Agent) VerifyRetainedThreads(ctx context.Context, owner provider.Ownership, runs []core.AgentRun) error {
	return a.verifyRetainedThreads(ctx, owner, runs, false)
}

func (a Agent) verifyRetainedThreads(ctx context.Context, owner provider.Ownership, runs []core.AgentRun, resume bool) error {
	return a.withSandboxAccess(ctx, owner, func(a Agent) error {
		return a.withServer(ctx, owner, func(p *protocol) error {
			return verifyRetainedHistory(ctx, p, runs, resume)
		})
	})
}

func verifyRetainedHistory(ctx context.Context, p *protocol, runs []core.AgentRun, resume bool) error {
	threads := make(map[string][]core.AgentRun)
	for _, run := range runs {
		threads[run.ThreadID] = append(threads[run.ThreadID], run)
	}
	for thread, expected := range threads {
		if thread == "" {
			return fmt.Errorf("native verification requires exact Thread identity")
		}
		if err := verifyRetainedThread(ctx, p, thread, expected, resume); err != nil {
			return err
		}
	}
	return nil
}

// VerifyUpgrade resumes each Thread and checks its exact native history to
// prove compatibility with an activated replacement package.
func (a Agent) VerifyUpgrade(ctx context.Context, owner provider.Ownership, runs []core.AgentRun) error {
	return a.verifyRetainedThreads(ctx, owner, runs, true)
}

func verifyRetainedThread(ctx context.Context, p *protocol, thread string, expected []core.AgentRun, resume bool) error {
	if resume {
		if err := p.resumeThread(ctx, thread); err != nil {
			return err
		}
	}
	turns, err := p.readTurns(ctx, thread)
	if err != nil {
		return err
	}
	return verifyRetainedTurns(expected, turns)
}

func verifyRetainedTurns(expected []core.AgentRun, turns []TurnOutcome) error {
	observed := make(map[string]string, len(turns))
	for _, turn := range turns {
		switch turn.Status {
		case "completed", "failed", "interrupted":
		default:
			return fmt.Errorf("native Thread is not quiescent")
		}
		observed[turn.ID] = turn.Status
	}
	for _, run := range expected {
		if run.TurnID != "" && (observed[run.TurnID] == "" || observed[run.TurnID] != run.TurnOutcome) {
			return fmt.Errorf("native verification did not retain exact settled Turn history")
		}
	}
	return nil
}

// Quiesce stops only the authenticated process already owned by Dorf.
// An absent server is already quiescent; this never starts or resumes one.
// Unknown or unauthenticated app-server processes are attention, not kill targets.
func (a Agent) Quiesce(ctx context.Context, owner provider.Ownership, runs []core.AgentRun) error {
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
			return nil
		}
		if !probe.tracked || probe.token == "" {
			return fmt.Errorf("cannot stop an unproven app-server")
		}
		p, err := dialProtocol(ctx, endpoint.DialURL, probe.token, endpoint.Headers(), endpoint.DialContext())
		if err != nil {
			return err
		}
		p.configureObservations(ctx, a.Observations, owner)
		defer a.finishProtocol(p)
		if err := verifyRetainedHistory(ctx, p, runs, false); err != nil {
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
