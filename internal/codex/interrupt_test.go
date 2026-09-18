package codex

import (
	"context"
	"errors"
	"github.com/aphronio/dorf/internal/core"
	"testing"
)

func TestNativeCancelCapturesTurnBeforeDispatchAndNeverRetargets(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[rejected], func(t *testing.T) {
			reads, interrupts := 0, 0
			captured := ""
			server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
				switch method {
				case "initialize":
					return map[string]any{}, false
				case "thread/read":
					reads++
					return map[string]any{"thread": map[string]any{"id": "thread", "turns": []any{map[string]any{"id": "old", "status": "completed"}, map[string]any{"id": "active", "status": "inProgress"}}}}, false
				case "turn/interrupt":
					interrupts++
					if captured != "active" || params["turnId"] != "active" {
						t.Error("cancel did not use the captured guard")
					}
					return map[string]any{}, rejected
				}
				t.Errorf("unexpected call %s", method)
				return nil, true
			})
			defer server.Close()
			ack := core.NativeAcknowledgement{ThreadID: "thread"}
			err := dialTestProtocol(t, server).cancelNative(context.Background(), &ack, core.NativeMutation{Begin: func(_ context.Context, turn string) (string, error) { captured = turn; return "", nil }})
			if (err != nil) != rejected || (rejected && !errors.Is(err, core.ErrNativeUnknown)) || reads != 1 || interrupts != 1 || ack.TurnID != "active" {
				t.Fatalf("ack=%+v err=%v reads=%d interrupts=%d", ack, err, reads, interrupts)
			}
		})
	}
}
