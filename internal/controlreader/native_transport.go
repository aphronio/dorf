package controlreader

import (
	"context"
	"errors"
	"net/http"

	"github.com/aphronio/dorf/internal/core"
)

const NativeEventPath = "/v1/sessions/events"
const NativeTurnsPath = "/v1/sessions/turns"
const MaxNativeEventBytes = 46 << 20

type nativeEventRequest struct {
	SessionID string           `json:"session_id"`
	Event     core.NativeEvent `json:"event"`
}

func (c Client) SubmitEvent(ctx context.Context, sessionID string, event core.NativeEvent) (core.NativeAcknowledgement, error) {
	response, err := c.request(ctx, NativeEventPath, nativeEventRequest{SessionID: sessionID, Event: event})
	if err != nil {
		return core.NativeAcknowledgement{}, errors.Join(core.ErrNativeUnknown, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return core.NativeAcknowledgement{}, decodeProblem(response)
	}
	var result core.NativeAcknowledgement
	if err := decodeJSONResponse(response, &result, MaxRequestBytes, "native acknowledgement"); err != nil {
		return result, errors.Join(core.ErrNativeUnknown, err)
	}
	return result, nil
}

func (c Client) ReadNativeTurns(ctx context.Context, sessionID string) (core.HarnessHistory, error) {
	response, err := c.request(ctx, NativeTurnsPath, observationRequest{SessionID: sessionID})
	if err != nil {
		return core.HarnessHistory{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return core.HarnessHistory{}, decodeProblem(response)
	}
	var result core.HarnessHistory
	err = decodeJSONResponse(response, &result, MaxObservationBytes, "native turns")
	return result, err
}

const InputCapabilitiesPath = "/v1/sessions/input-capabilities"

func (c Client) InputCapabilities(ctx context.Context, sessionID string) (core.InputCapabilities, error) {
	response, err := c.request(ctx, InputCapabilitiesPath, observationRequest{SessionID: sessionID})
	if err != nil {
		return core.InputCapabilities{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return core.InputCapabilities{}, decodeProblem(response)
	}
	var result core.InputCapabilities
	err = decodeJSONResponse(response, &result, MaxRequestBytes, "input capabilities")
	return result, err
}
