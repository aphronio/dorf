package codex

import (
	"context"
	"fmt"

	provider "github.com/aphronio/dorf/internal/sandbox"
	terminalapp "github.com/aphronio/dorf/internal/terminal"
)

// nativeOperation is callback-owned and used sequentially by Core's contract.
// Control ownership ends before a durable wait or another claim; an accepted
// observation may take ownership of the transport at scope exit.
type nativeOperation struct {
	ctx           context.Context
	stopTransport func()
	threadID      string
	owner         provider.Ownership
	original      Agent
	protocol      *protocol
	closed        bool
	invalid       bool
	accessErr     error
}

func (a Agent) WithOperation(ctx context.Context, owner provider.Ownership, threadID string, fn func(context.Context, terminalapp.Harness) error) error {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	operation := &nativeOperation{ctx: ctx, owner: owner, original: a, threadID: threadID}
	defer func() {
		operation.closed = true
		if operation.protocol != nil {
			operation.finish()
		}
	}()
	entered := false
	err := a.withSandboxAccess(ctx, owner, func(bound Agent) error {
		entered = true
		bound.operation = operation
		return fn(ctx, bound)
	})
	if entered {
		return err
	}
	// Surface acquisition failure through History so Core retains its normal
	// attention/recovery handling rather than bypassing the durable contract.
	operation.accessErr = err
	a.operation = operation
	return fn(ctx, a)
}

func (a Agent) finishProtocol(p *protocol) {
	if a.operation != nil {
		a.operation.protocol = p
		stopped := make(chan struct{})
		stop := context.AfterFunc(a.operation.ctx, func() {
			defer close(stopped)
			p.connection.CloseNow()
		})
		a.operation.stopTransport = func() {
			if !stop() {
				<-stopped
			}
		}
		return
	}
	p.finish()
}

func (s *nativeOperation) check(ctx context.Context, owner provider.Ownership) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return fmt.Errorf("native operation scope has ended")
	}
	if owner != s.owner {
		return fmt.Errorf("native operation requires its exact Sandbox owner")
	}
	return s.accessErr
}

func (s *nativeOperation) use(ctx context.Context, a Agent, owner provider.Ownership, fn func(*protocol) error) error {
	if err := s.check(ctx, owner); err != nil {
		return err
	}
	if s.invalid {
		// Only the contract's read-after-error reconciliation may acquire another
		// connection. Never replay a mutation over a refreshed connection.
		return fmt.Errorf("native operation transport is invalid")
	}
	var err error
	if s.protocol == nil {
		err = a.openServer(ctx, owner, fn)
	} else {
		s.protocol.configureObservations(ctx, a.Observations, owner)
		err = fn(s.protocol)
	}
	if err != nil {
		s.invalid = true
		if s.protocol != nil {
			s.finish()
		}
	}
	return err
}

// Stop cancellation's control ownership before finish can transfer the socket
// to the independent observation context. If cancellation already won, wait
// for CloseNow to finish before releasing the surrounding operation fence.
func (s *nativeOperation) finish() {
	if s.stopTransport != nil {
		s.stopTransport()
		s.stopTransport = nil
	}
	s.protocol.finish()
	s.protocol = nil
}
