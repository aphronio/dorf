package e2b

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	process "github.com/aphronio/dorf/internal/e2b/gen/process"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

const commandStopTimeout = 5 * time.Second

func (a Adapter) Run(ctx context.Context, owner provider.Ownership, command provider.RunRequest) (result provider.RunResult, err error) {
	err = a.WithAccess(ctx, owner, func(s provider.Sandbox) error {
		result, err = s.Run(ctx, owner, command)
		return err
	})
	return result, err
}

func (s *scopedAccess) Run(ctx context.Context, owner provider.Ownership, command provider.RunRequest) (provider.RunResult, error) {
	ctx, done, err := s.operation(ctx, owner)
	if err != nil {
		return provider.RunResult{}, err
	}
	defer done()
	return s.executor.Run(ctx, command)
}

// Run builds on the existing envd Start stream and signal API. It never
// relaunches a command after ambiguous observation and never persists Env.
func (e *Executor) Run(ctx context.Context, command provider.RunRequest) (provider.RunResult, error) {
	if command.Timeout <= 0 || command.Timeout > 24*time.Hour {
		return provider.RunResult{}, fmt.Errorf("command requires a bounded positive timeout")
	}
	if err := ctx.Err(); err != nil {
		return provider.RunResult{Stopped: true}, err
	}
	stdout := provider.OutputBuffer{Limit: command.MaxOutputBytes}
	stderr := provider.OutputBuffer{Limit: command.MaxOutputBytes}
	observed, err := e.Exec(ctx, ExecRequest{Argv: command.Args, Stdin: command.Stdin, Env: command.Env,
		ProcessTimeout: command.Timeout, Stdout: &stdout, Stderr: &stderr})
	result, execErr := providerExecResult(observed, stdout.String(), stderr.String(), err)
	result.Truncated = stdout.Truncated || stderr.Truncated
	out := provider.RunResult{Result: result}
	var exit *ExitError
	if err == nil || errors.As(err, &exit) {
		out.Stopped = true
		return out, execErr
	}
	if observed.PID == 0 {
		return out, errors.Join(ctx.Err(), execErr)
	}
	started := time.Now()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandStopTimeout)
	defer cancel()
	stopErr := e.stopCommand(stopCtx, observed.PID)
	out.Stopped, out.StopDuration = stopErr == nil, time.Since(started)
	var timeout *ProcessTimeoutError
	if errors.As(err, &timeout) {
		execErr = errors.Join(provider.ErrCommandTimeout, execErr)
	}
	return out, errors.Join(ctx.Err(), execErr, stopErr)
}

func (e *Executor) stopCommand(ctx context.Context, pid uint32) error {
	// A lost signal acknowledgement can still be resolved by observing absence.
	_ = e.Kill(ctx, pid)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := e.process.List(ctx, connect.NewRequest(&process.ListRequest{}))
		if err != nil {
			return fmt.Errorf("remote command stop could not be confirmed")
		}
		present := false
		for _, info := range response.Msg.Processes {
			if info.Pid == pid {
				present = true
				break
			}
		}
		if !present {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("remote command did not stop within cancellation bound")
		case <-ticker.C:
		}
	}
}
