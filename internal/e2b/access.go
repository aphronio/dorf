package e2b

import (
	"bytes"
	"context"
	"sync"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

// WithAccess resolves one exact provider resource and its scoped capabilities
// for a synchronous operation. It never caches across callbacks or retries a
// command after an ambiguous response. The caller owns the surrounding Job fence.
func (a Adapter) WithAccess(ctx context.Context, owner provider.Ownership, fn func(provider.Sandbox) error) error {
	owned, err := a.Client.FindOwned(ctx, e2bOwnership(owner))
	if err != nil {
		return err
	}
	if owned == nil {
		return provider.OwnershipErrorf("E2B Sandbox metadata is missing, foreign, stale, or ambiguous")
	}
	response, err := a.Client.connect(ctx, owned.ProviderID, a.Config.SandboxTimeout)
	if err != nil {
		return err
	}
	connection, err := envdConnection(owned.ProviderID, response)
	if err != nil {
		return err
	}
	executor, err := NewExecutor(connection, a.Client.HTTPClient)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, a.Config.SandboxTimeout)
	scope := &scopedAccess{Sandbox: a, adapter: a, owner: owner, providerID: owned.ProviderID, response: response, executor: executor, ctx: ctx}
	defer func() {
		scope.mu.Lock()
		scope.closed = true
		scope.mu.Unlock()
		cancel()
		scope.active.Wait()
	}()
	return fn(scope)
}

// Embedding only the baseline interface deliberately prevents nested access
// scopes. Lifecycle operations still use the original adapter's fresh checks.
type scopedAccess struct {
	provider.Sandbox
	adapter    Adapter
	owner      provider.Ownership
	providerID string
	response   connectionResponse
	executor   *Executor
	ctx        context.Context
	mu         sync.Mutex
	closed     bool
	active     sync.WaitGroup
}

func (s *scopedAccess) operation(ctx context.Context, owner provider.Ownership) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner != s.owner {
		return nil, nil, provider.OwnershipErrorf("E2B access scope belongs to a different Sandbox owner")
	}
	if s.closed {
		return nil, nil, context.Canceled
	}
	if err := s.ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.active.Add(1)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() { stop(); cancel(); s.active.Done() }, nil
}

func (s *scopedAccess) Exec(ctx context.Context, owner provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	ctx, done, err := s.operation(ctx, owner)
	if err != nil {
		return provider.Result{}, err
	}
	defer done()
	var stdout, stderr bytes.Buffer
	result, execErr := s.executor.Exec(ctx, ExecRequest{Argv: append([]string(nil), args...), Stdin: input, ProcessTimeout: s.adapter.Config.ProcessTimeout, Stdout: &stdout, Stderr: &stderr})
	return providerExecResult(result, stdout.String(), stderr.String(), execErr)
}

func (s *scopedAccess) Endpoint(ctx context.Context, owner provider.Ownership, port int) (provider.Endpoint, error) {
	_, done, err := s.operation(ctx, owner)
	if err != nil {
		return provider.Endpoint{}, err
	}
	defer done()
	endpoint, err := connectionEndpoint(s.providerID, port, s.response)
	if err != nil {
		return provider.Endpoint{}, err
	}
	return provider.NewEndpoint(endpoint.ListenURL, endpoint.DialURL, endpoint.Headers()), nil
}

func (s *scopedAccess) ReadFile(ctx context.Context, owner provider.Ownership, name string) ([]byte, error) {
	return provider.ReadFileViaExec(ctx, owner, s.Workspace(), name, s.Exec)
}

func (s *scopedAccess) PutFile(ctx context.Context, owner provider.Ownership, name string, contents []byte) error {
	return provider.PutFileViaExec(ctx, owner, name, contents, s.Exec)
}

func (s *scopedAccess) ReadFiles(ctx context.Context, owner provider.Ownership, names []string, maxBytes int) (map[string][]byte, error) {
	return provider.ReadFilesViaExec(ctx, owner, s.Workspace(), names, maxBytes, s.Exec)
}
