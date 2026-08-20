package absurdruntime

import (
	"context"
	"fmt"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type factStepResultV1 struct {
	FactID string `json:"fact_id"`
}

// RunFactStep checkpoints one stable workflow fact while keeping its Absurd
// claim alive across opaque work. The caller owns the fact's meaning and the
// operation that records it.
func RunFactStep(ctx context.Context, name, factID string, work func(context.Context) error) error {
	result, err := absurd.Step(ctx, name, func(stepCtx context.Context) (factStepResultV1, error) {
		return WithHeartbeat(stepCtx, func(workCtx context.Context) (factStepResultV1, error) {
			if err := work(workCtx); err != nil {
				return factStepResultV1{}, err
			}
			return factStepResultV1{FactID: factID}, nil
		})
	})
	if err != nil {
		return err
	}
	if result.FactID != factID {
		return fmt.Errorf("Step %s returned fact %q, want %q", name, result.FactID, factID)
	}
	return nil
}
