package controlreader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

type nativeFixture struct {
	*readerTestStore
	state   core.NativeState
	history core.HarnessHistory
	calls   int
	event   core.NativeEvent
	lost    bool
}

func (f *nativeFixture) NativeState(context.Context, string) (core.NativeState, error) {
	return f.state, nil
}
func (f *nativeFixture) BindNativeThread(_ context.Context, _, thread string) error {
	f.session.ThreadID = thread
	return nil
}
func (f *nativeFixture) BeginNativeMutation(_ context.Context, _, thread, input, turn string) (int64, error) {
	if !f.inFence || f.state.Pending() || thread != f.session.ThreadID {
		return 0, errors.New("invalid native custody")
	}
	f.state.Revision++
	f.state.PendingInputID = input
	f.state.PendingTurnID = turn
	return f.state.Revision, nil
}
func (f *nativeFixture) FinishNativeMutation(_ context.Context, _ string, revision int64) error {
	if revision != f.state.Revision {
		return errors.New("stale completion")
	}
	f.state.PendingInputID = ""
	f.state.PendingTurnID = ""
	return nil
}
func (f *nativeFixture) Actions(context.Context, string) ([]core.Action, error) {
	return []core.Action{{Kind: core.ActionSandboxCreate, Scope: f.sandbox.ID, State: core.ActionSucceeded}, {Kind: core.ActionRouteCreate, Scope: f.sandbox.ID, State: core.ActionSucceeded}}, nil
}
func (f *nativeFixture) ResolveSandbox(_ context.Context, profile core.SandboxProfileRef) (core.SandboxRuntime, error) {
	return core.SandboxRuntime{SandboxProfile: profile, Native: f}, nil
}
func (f *nativeFixture) SubmitEvent(ctx context.Context, _ core.Session, _ core.Sandbox, e core.NativeEvent, m core.NativeMutation) (core.NativeAcknowledgement, error) {
	f.calls++
	f.event = e
	if err := m.Bind(ctx, "thread"); err != nil {
		return core.NativeAcknowledgement{}, err
	}
	if _, err := m.Begin(ctx, ""); err != nil {
		return core.NativeAcknowledgement{}, err
	}
	if f.lost {
		return core.NativeAcknowledgement{}, errors.New("response lost")
	}
	return core.NativeAcknowledgement{Harness: "codex", Type: "input.accepted", ClientID: e.ClientID, ThreadID: "thread", TurnID: "turn"}, nil
}
func (f *nativeFixture) ReadNativeTurns(context.Context, core.Session, core.Sandbox) (core.HarnessHistory, error) {
	return f.history, nil
}
func (f *nativeFixture) NativeIdle(context.Context, core.Session, core.Sandbox) (bool, error) {
	return true, nil
}
func TestNativeLostAcknowledgementRequiresExactPositiveEvidence(t *testing.T) {
	f := &nativeFixture{readerTestStore: &readerTestStore{session: core.Session{ID: "session", Harness: "codex", AdmissionOpen: true, CleanupState: core.CleanupPending}, sandbox: core.Sandbox{ID: core.MainSandboxName("session"), SessionID: "session", OwnershipNonce: "nonce"}}, lost: true}
	service := Service{Store: f, Runtimes: f}
	event := core.NativeEvent{Type: core.InputMessage, ClientID: "same", Text: "input"}
	ctx := context.Background()
	if _, err := service.SubmitEvent(ctx, "session", event); !errors.Is(err, core.ErrNativeUnknown) {
		t.Fatal(err)
	}
	first := f.state.PendingInputID
	f.history = core.HarnessHistory{Turns: []core.HarnessTurn{{ID: "turn", Status: "completed", ClientIDs: []string{"same/older"}}}}
	if _, err := service.SubmitEvent(ctx, "session", event); !errors.Is(err, core.ErrNativeUnknown) || f.calls != 1 {
		t.Fatalf("unproved resend: calls=%d err=%v", f.calls, err)
	}
	f.history.Turns[0].ClientIDs = []string{first}
	if _, err := service.SubmitEvent(ctx, "session", event); !errors.Is(err, core.ErrNativeUnknown) || f.calls != 2 || f.state.PendingInputID == first {
		t.Fatalf("fresh dispatch: %+v %v", f.state, err)
	}
	f.deliveryHeld = true
	if _, err := service.SubmitEvent(ctx, "session", event); !errors.Is(err, core.ErrNativeUnavailable) || f.calls != 2 {
		t.Fatalf("held dispatch: %v", err)
	}
}

func TestNativeAttachmentTransportPreservesTenAndRejectsEleven(t *testing.T) {
	f := &nativeFixture{readerTestStore: &readerTestStore{session: core.Session{ID: "session", Harness: "codex", AdmissionOpen: true, CleanupState: core.CleanupPending}, sandbox: core.Sandbox{ID: core.MainSandboxName("session"), SessionID: "session", OwnershipNonce: "nonce"}}}
	token := strings.Repeat("b", 64)
	handler, err := NewHandler(token, Service{Store: f, Runtimes: f})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient("http://control-reader.test:8756", token, &http.Client{Transport: readerHandlerTransport{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	event := core.NativeEvent{Type: core.InputMessage, ClientID: "ten-files"}
	for i := range 10 {
		event.Attachments = append(event.Attachments, core.NativeAttachment{Filename: fmt.Sprintf("file-%02d.bin", i), Contents: bytes.Repeat([]byte{byte(i)}, 1<<20)})
	}
	ack, err := client.SubmitEvent(context.Background(), "session", event)
	if err != nil || ack.Type != "input.accepted" || f.calls != 1 || !reflect.DeepEqual(f.event, event) || f.state.Pending() {
		t.Fatalf("ten-file transport: ack=%+v calls=%d pending=%v err=%v", ack, f.calls, f.state.Pending(), err)
	}
	event.Attachments = append(event.Attachments, core.NativeAttachment{Filename: "eleven.bin"})
	if _, err := client.SubmitEvent(context.Background(), "session", event); err == nil || f.calls != 1 {
		t.Fatalf("eleven-file transport: calls=%d err=%v", f.calls, err)
	}
	event.Attachments = event.Attachments[:10]
	for i := range event.Attachments {
		event.Attachments[i].Contents = make([]byte, 4<<20)
	}
	if _, err := client.SubmitEvent(context.Background(), "session", event); err == nil || f.calls != 1 {
		t.Fatalf("oversized transport: calls=%d err=%v", f.calls, err)
	}
}

func (f *nativeFixture) InputCapabilities(context.Context, core.Session, core.Sandbox) (core.InputCapabilities, error) {
	return core.InputCapabilities{Model: "model", AudioMediaTypes: []string{}}, nil
}
