package controlreader

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func observationStreamEndpoint(service Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input observationRequest
		if !decodeRequest(w, r, &input) {
			return
		}
		started := false
		controller := http.NewResponseController(w)
		err := service.StreamTurnObservation(r.Context(), input.SessionID, input.TurnID, input.Cursor, func(value TurnObservation) error {
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if len(raw) > MaxObservationBytes {
				return ErrResponseTooLarge
			}
			if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return err
			}
			if !started {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				started = true
			}
			if _, err := w.Write(append(raw, '\n')); err != nil {
				return err
			}
			if err := controller.Flush(); err != nil {
				return err
			}
			return controller.SetWriteDeadline(time.Time{})
		})
		if err != nil && !started && r.Context().Err() == nil {
			writeServiceError(w, err)
		}
	}
}

func (c Client) ReadTurnObservation(ctx context.Context, sessionID, turnID, cursor string) (TurnObservation, error) {
	response, err := c.request(ctx, CoherentObservationPath, observationRequest{SessionID: sessionID, TurnID: turnID, Cursor: cursor})
	if err != nil {
		return TurnObservation{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return TurnObservation{}, decodeProblem(response)
	}
	var value TurnObservation
	err = decodeJSONResponse(response, &value, MaxObservationBytes, "message observation")
	return value, err
}

func (c Client) StreamTurnObservation(ctx context.Context, sessionID, turnID, cursor string, emit func(TurnObservation) error) error {
	client := *c.http
	client.Timeout = ObservationStreamTimeout + 5*time.Second
	c.http = &client
	response, err := c.request(ctx, ObservationStreamPath, observationRequest{SessionID: sessionID, TurnID: turnID, Cursor: cursor})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeProblem(response)
	}
	if response.Header.Get("Content-Type") != "application/x-ndjson" {
		return fmt.Errorf("control reader returned invalid stream content type")
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), MaxObservationBytes+1)
	for scanner.Scan() {
		var value TurnObservation
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return ErrUnavailable
		}
		if err := emit(value); err != nil {
			return err
		}
	}
	return scanner.Err()
}
