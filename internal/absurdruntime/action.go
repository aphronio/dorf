package absurdruntime

import (
	"context"
	"fmt"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type actionStepResultV1 struct {
	ActionID string `json:"action_id"`
}

// RunActionStep checkpoints one stable Dorf Action while keeping its Absurd
// claim alive across opaque external work.
func RunActionStep(ctx context.Context, actionID string, work func(context.Context) error) error {
	name := actionStepName(actionID)
	result, err := absurd.Step(ctx, name, func(stepCtx context.Context) (actionStepResultV1, error) {
		return WithHeartbeat(stepCtx, func(workCtx context.Context) (actionStepResultV1, error) {
			if err := work(workCtx); err != nil {
				return actionStepResultV1{}, err
			}
			return actionStepResultV1{ActionID: actionID}, nil
		})
	})
	if err != nil {
		return err
	}
	if result.ActionID != actionID {
		return fmt.Errorf("Step %s returned Action %q, want %q", name, result.ActionID, actionID)
	}
	return nil
}

func actionStepName(actionID string) string { return "dorf/action/v1/" + actionID }
