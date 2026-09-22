package controlreader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
)

type nativeStore interface {
	NativeState(context.Context, string) (core.NativeState, error)
	BindNativeThread(context.Context, string, string) error
	BeginNativeMutation(context.Context, string, string, string, string) (int64, error)
	FinishNativeMutation(context.Context, string, int64) error
	Actions(context.Context, string) ([]core.Action, error)
}

func (s Service) SubmitEvent(ctx context.Context, sessionID string, event core.NativeEvent) (core.NativeAcknowledgement, error) {
	if err := event.Validate(); err != nil {
		return core.NativeAcknowledgement{}, err
	}
	store, ok := s.Store.(nativeStore)
	if !ok || !validIdentity(sessionID) {
		return core.NativeAcknowledgement{}, core.ErrNativeUnavailable
	}
	var ack core.NativeAcknowledgement
	err := s.withSandbox(ctx, core.MainSandboxName(sessionID), func(runtime core.SandboxRuntime, session core.Session, owned core.Sandbox) error {
		if runtime.Native == nil || !session.AdmissionOpen {
			return core.ErrNativeUnavailable
		}
		actions, err := store.Actions(ctx, sessionID)
		if err != nil {
			return err
		}
		if !core.HasSucceededAction(actions, core.ActionSandboxCreate, owned.ID) || !core.HasSucceededAction(actions, core.ActionRouteCreate, owned.ID) {
			return core.ErrNativeUnavailable
		}
		state, err := store.NativeState(ctx, sessionID)
		if err != nil {
			return err
		}
		if state.Pending() {
			history, err := runtime.Native.ReadNativeTurns(ctx, session, owned)
			if err != nil || !core.NativeMutationProven(state, history) {
				return core.ErrNativeUnknown
			}
			if err := store.FinishNativeMutation(ctx, sessionID, state.Revision); err != nil {
				return err
			}
		}
		ack, err = submitNativeEvent(ctx, store, runtime.Native, session, owned, event, state)
		return err
	})
	if errors.Is(err, ErrUnavailable) {
		err = core.ErrNativeUnavailable
	}
	return ack, err
}

func (s Service) ReadNativeTurns(ctx context.Context, sessionID string) (core.HarnessHistory, error) {
	if !validIdentity(sessionID) {
		return core.HarnessHistory{}, ErrSessionNotFound
	}
	var result core.HarnessHistory
	err := s.withSandbox(ctx, core.MainSandboxName(sessionID), func(runtime core.SandboxRuntime, session core.Session, owned core.Sandbox) error {
		if runtime.Native == nil {
			return core.ErrNativeUnavailable
		}
		var err error
		result, err = runtime.Native.ReadNativeTurns(ctx, session, owned)
		return err
	})
	if err == nil {
		for i := range result.Turns {
			for j, id := range result.Turns[i].ClientIDs {
				result.Turns[i].ClientIDs[j], _, _ = strings.Cut(id, "/")
			}
		}
	}
	return result, err
}

func submitNativeEvent(ctx context.Context, store nativeStore, native core.NativeSession, session core.Session, owned core.Sandbox, event core.NativeEvent, state core.NativeState) (core.NativeAcknowledgement, error) {
	var revision int64
	mutation := core.NativeMutation{
		Unused: state.Revision == 0,
		Bind: func(ctx context.Context, threadID string) error {
			if err := store.BindNativeThread(ctx, session.ID, threadID); err != nil {
				return err
			}
			session.ThreadID = threadID
			return nil
		},
		Begin: func(ctx context.Context, turnID string) (string, error) {
			nonce := make([]byte, 16)
			if _, err := rand.Read(nonce); err != nil {
				return "", err
			}
			inputID := event.ClientID + "/" + hex.EncodeToString(nonce)
			if turnID != "" {
				inputID = ""
			}
			var err error
			revision, err = store.BeginNativeMutation(ctx, session.ID, session.ThreadID, inputID, turnID)
			return inputID, err
		},
	}
	ack, err := native.SubmitEvent(ctx, session, owned, event, mutation)
	if err != nil {
		if revision != 0 {
			return ack, errors.Join(core.ErrNativeUnknown, err)
		}
		return ack, err
	}
	if revision == 0 {
		return ack, nil
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.FinishNativeMutation(finishCtx, session.ID, revision); err != nil {
		return ack, errors.Join(core.ErrNativeUnknown, err)
	}
	return ack, nil
}

func (s Service) InputCapabilities(ctx context.Context, sessionID string) (core.InputCapabilities, error) {
	if !validIdentity(sessionID) {
		return core.InputCapabilities{}, ErrSessionNotFound
	}
	var result core.InputCapabilities
	err := s.accessSandbox(ctx, core.MainSandboxName(sessionID), false, func(runtime core.SandboxRuntime, session core.Session, owned core.Sandbox) error {
		if runtime.Native == nil {
			return core.ErrNativeUnavailable
		}
		var err error
		result, err = runtime.Native.InputCapabilities(ctx, session, owned)
		return err
	})
	if errors.Is(err, ErrUnavailable) {
		err = core.ErrNativeUnavailable
	}
	return result, err
}
