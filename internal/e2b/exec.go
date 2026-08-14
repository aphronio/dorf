package e2b

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	process "github.com/aphronio/dorf/internal/e2b/gen/process"
	"github.com/aphronio/dorf/internal/e2b/gen/process/processconnect"
)

const (
	envdPort           = "49983"
	minimumEnvdVersion = "0.5.2"
)

type ExecRequest struct {
	Argv           []string
	Stdin          []byte
	Cwd            string
	Env            map[string]string
	ProcessTimeout time.Duration
	Stdout         io.Writer
	Stderr         io.Writer
}

type ExecResult struct {
	PID         uint32
	ExitCode    int32
	Exited      bool
	Status      string
	RemoteError string
}

type ExitError struct{ Result ExecResult }

func (e *ExitError) Error() string {
	if e.Result.RemoteError != "" {
		return fmt.Sprintf("E2B process %d failed with exit code %d: %s", e.Result.PID, e.Result.ExitCode, e.Result.RemoteError)
	}
	return fmt.Sprintf("E2B process %d failed with exit code %d", e.Result.PID, e.Result.ExitCode)
}

type ProcessTimeoutError struct {
	PID     uint32
	Timeout time.Duration
}

func (e *ProcessTimeoutError) Error() string {
	return fmt.Sprintf("E2B process %d exceeded its %s process timeout", e.PID, e.Timeout)
}

// IndeterminateExecError means envd may still be running the process but Dorf
// did not observe its terminal event. Callers must not replay the command.
type IndeterminateExecError struct {
	PID   uint32
	Cause error
}

func (e *IndeterminateExecError) Error() string {
	if e.PID == 0 {
		return fmt.Sprintf("E2B process start is indeterminate: %v", e.Cause)
	}
	return fmt.Sprintf("E2B process %d is indeterminate: %v", e.PID, e.Cause)
}

func (e *IndeterminateExecError) Unwrap() error { return e.Cause }

type Executor struct{ process envdProcessClient }

func NewExecutor(connection EnvdConnection, client HTTPClient) (*Executor, error) {
	if strings.TrimSpace(connection.ProviderID) == "" || strings.TrimSpace(connection.Domain) == "" || connection.accessToken == "" {
		return nil, fmt.Errorf("E2B envd connection is incomplete")
	}
	if compareNumericVersion(connection.Version, minimumEnvdVersion) < 0 {
		return nil, fmt.Errorf("E2B envd %s is unsupported; need at least %s", connection.Version, minimumEnvdVersion)
	}
	baseURL, err := envdBaseURL(connection)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	authenticated := envdHTTPClient{
		next:        client,
		providerID:  connection.ProviderID,
		accessToken: connection.accessToken,
	}
	generated := processconnect.NewProcessClient(authenticated, baseURL)
	return &Executor{process: generatedProcessClient{ProcessClient: generated}}, nil
}

func (e *Executor) Exec(ctx context.Context, request ExecRequest) (ExecResult, error) {
	if e == nil || e.process == nil {
		return ExecResult{}, fmt.Errorf("E2B executor is not configured")
	}
	if len(request.Argv) == 0 || strings.TrimSpace(request.Argv[0]) == "" {
		return ExecResult{}, fmt.Errorf("E2B exec requires a non-empty argv")
	}
	if request.ProcessTimeout < 0 || request.ProcessTimeout%time.Millisecond != 0 {
		return ExecResult{}, fmt.Errorf("E2B process timeout must be zero or a positive whole number of milliseconds")
	}
	stdin := request.Stdin != nil
	config := &process.ProcessConfig{
		Cmd:  request.Argv[0],
		Args: append([]string(nil), request.Argv[1:]...),
		Envs: cloneStrings(request.Env),
	}
	if request.Cwd != "" {
		config.Cwd = &request.Cwd
	}
	message := connect.NewRequest(&process.StartRequest{Process: config, Stdin: &stdin})
	message.Header().Set("Keepalive-Ping-Interval", "50")
	if request.ProcessTimeout > 0 {
		message.Header().Set("Connect-Timeout-Ms", strconv.FormatInt(request.ProcessTimeout.Milliseconds(), 10))
	}
	// Connect-Go otherwise replaces our explicit process timeout header with
	// the caller's observation deadline. Keep cancellation but hide that
	// deadline so envd receives the process lifetime chosen above.
	stream, err := e.process.Start(observationContext{Context: ctx}, message)
	if err != nil {
		return ExecResult{}, &IndeterminateExecError{Cause: err}
	}
	defer stream.Close()

	stdout := request.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := request.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	var result ExecResult
	var inputDone <-chan error
	for stream.Receive() {
		response := stream.Msg()
		if response == nil || response.Event == nil {
			return result, indeterminate(result.PID, fmt.Errorf("envd returned an empty process event"))
		}
		switch event := response.Event.Event.(type) {
		case *process.ProcessEvent_Start:
			if result.PID != 0 || event.Start == nil || event.Start.Pid == 0 {
				return result, indeterminate(result.PID, fmt.Errorf("envd returned an invalid or duplicate start event"))
			}
			result.PID = event.Start.Pid
			if stdin {
				inputDone = e.sendStdin(ctx, result.PID, request.Stdin)
			}
		case *process.ProcessEvent_Data:
			if result.PID == 0 || event.Data == nil {
				return result, indeterminate(result.PID, fmt.Errorf("envd returned process data before start"))
			}
			if err := writeProcessData(stdout, stderr, event.Data); err != nil {
				return result, indeterminate(result.PID, err)
			}
		case *process.ProcessEvent_Keepalive:
			if result.PID == 0 {
				return result, indeterminate(0, fmt.Errorf("envd returned keepalive before start"))
			}
		case *process.ProcessEvent_End:
			if result.PID == 0 || event.End == nil {
				return result, indeterminate(result.PID, fmt.Errorf("envd returned end before start"))
			}
			if inputDone != nil {
				select {
				case inputErr := <-inputDone:
					if inputErr != nil {
						return result, indeterminate(result.PID, fmt.Errorf("deliver stdin: %w", inputErr))
					}
				case <-ctx.Done():
					return result, indeterminate(result.PID, ctx.Err())
				}
			}
			result.ExitCode = event.End.ExitCode
			result.Exited = event.End.Exited
			result.Status = event.End.Status
			result.RemoteError = event.End.GetError()
			if result.ExitCode != 0 || !result.Exited || result.RemoteError != "" {
				return result, &ExitError{Result: result}
			}
			return result, nil
		default:
			return result, indeterminate(result.PID, fmt.Errorf("envd returned an unknown process event"))
		}
	}
	streamErr := stream.Err()
	if streamErr == nil {
		streamErr = io.ErrUnexpectedEOF
	}
	if result.PID != 0 && request.ProcessTimeout > 0 && ctx.Err() == nil && connect.CodeOf(streamErr) == connect.CodeDeadlineExceeded {
		return result, &ProcessTimeoutError{PID: result.PID, Timeout: request.ProcessTimeout}
	}
	return result, indeterminate(result.PID, streamErr)
}

// Kill sends one SIGKILL request. It is deliberately not retried after an
// ambiguous transport failure.
func (e *Executor) Kill(ctx context.Context, pid uint32) error {
	if e == nil || e.process == nil {
		return fmt.Errorf("E2B executor is not configured")
	}
	if pid == 0 {
		return fmt.Errorf("E2B process PID is required")
	}
	request := connect.NewRequest(&process.SendSignalRequest{
		Process: processByPID(pid),
		Signal:  process.Signal_SIGNAL_SIGKILL,
	})
	_, err := e.process.SendSignal(ctx, request)
	return err
}

func (e *Executor) sendStdin(ctx context.Context, pid uint32, input []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		if len(input) > 0 {
			_, err := e.process.SendInput(ctx, connect.NewRequest(&process.SendInputRequest{
				Process: processByPID(pid),
				Input: &process.ProcessInput{Input: &process.ProcessInput_Stdin{
					Stdin: append([]byte(nil), input...),
				}},
			}))
			if err != nil {
				done <- err
				return
			}
		}
		_, err := e.process.CloseStdin(ctx, connect.NewRequest(&process.CloseStdinRequest{Process: processByPID(pid)}))
		// The command may consume the complete input and exit before envd handles
		// CloseStdin. SendInput has already succeeded in that case, and the Start
		// stream's terminal event remains the authoritative process result.
		if connect.CodeOf(err) == connect.CodeNotFound {
			err = nil
		}
		done <- err
	}()
	return done
}

func processByPID(pid uint32) *process.ProcessSelector {
	return &process.ProcessSelector{Selector: &process.ProcessSelector_Pid{Pid: pid}}
}

func writeProcessData(stdout, stderr io.Writer, data *process.ProcessEvent_DataEvent) error {
	switch output := data.Output.(type) {
	case *process.ProcessEvent_DataEvent_Stdout:
		return writeExact(stdout, output.Stdout)
	case *process.ProcessEvent_DataEvent_Stderr:
		return writeExact(stderr, output.Stderr)
	case *process.ProcessEvent_DataEvent_Pty:
		return fmt.Errorf("envd returned PTY data for non-PTY exec")
	default:
		return fmt.Errorf("envd returned data without an output kind")
	}
}

func writeExact(writer io.Writer, data []byte) error {
	written, err := writer.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func indeterminate(pid uint32, cause error) error {
	return &IndeterminateExecError{PID: pid, Cause: cause}
}

func cloneStrings(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func envdBaseURL(connection EnvdConnection) (string, error) {
	domain := strings.TrimSpace(connection.Domain)
	if strings.ContainsAny(domain, "/:") {
		return "", fmt.Errorf("E2B Sandbox domain %q is invalid", domain)
	}
	host := envdPort + "-" + connection.ProviderID + "." + domain
	for _, shared := range []string{"e2b.app", "e2b.dev", "e2b.pro", "e2b-staging.dev"} {
		if domain == shared {
			host = "sandbox." + domain
			break
		}
	}
	endpoint := &url.URL{Scheme: "https", Host: host}
	return endpoint.String(), nil
}

func compareNumericVersion(left, right string) int {
	leftParts := strings.Split(strings.TrimPrefix(left, "v"), ".")
	rightParts := strings.Split(strings.TrimPrefix(right, "v"), ".")
	for i := range max(len(leftParts), len(rightParts)) {
		var leftValue, rightValue int
		if i < len(leftParts) {
			leftValue, _ = strconv.Atoi(leftParts[i])
		}
		if i < len(rightParts) {
			rightValue, _ = strconv.Atoi(rightParts[i])
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

type envdHTTPClient struct {
	next        HTTPClient
	providerID  string
	accessToken string
}

func (c envdHTTPClient) Do(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header = request.Header.Clone()
	cloned.Header.Set("E2b-Sandbox-Id", c.providerID)
	cloned.Header.Set("E2b-Sandbox-Port", envdPort)
	cloned.Header.Set("X-Access-Token", c.accessToken)
	return c.next.Do(cloned)
}

type startStream interface {
	Receive() bool
	Msg() *process.StartResponse
	Err() error
	Close() error
}

type envdProcessClient interface {
	Start(context.Context, *connect.Request[process.StartRequest]) (startStream, error)
	SendInput(context.Context, *connect.Request[process.SendInputRequest]) (*connect.Response[process.SendInputResponse], error)
	SendSignal(context.Context, *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error)
	CloseStdin(context.Context, *connect.Request[process.CloseStdinRequest]) (*connect.Response[process.CloseStdinResponse], error)
}

type generatedProcessClient struct{ processconnect.ProcessClient }

func (c generatedProcessClient) Start(ctx context.Context, request *connect.Request[process.StartRequest]) (startStream, error) {
	return c.ProcessClient.Start(ctx, request)
}

type observationContext struct{ context.Context }

func (observationContext) Deadline() (time.Time, bool) { return time.Time{}, false }
