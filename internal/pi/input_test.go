package pi

import (
	"context"
	"errors"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestPiRejectsImagesBeforeStartingNativeWork(t *testing.T) {
	input := core.HarnessInput{Images: []core.HarnessImage{{MediaType: "image/png", Bytes: []byte("image")}}}
	for _, route := range []string{"initial", "follow", "steer"} {
		t.Run(route, func(t *testing.T) {
			agent := Agent{}
			ctx, owner := context.Background(), testOwner("sandbox")
			var err error
			switch route {
			case "initial":
				_, err = agent.StartInitialTurn(ctx, owner, "/workspace/job", "run", input, "model", "high", false)
			case "follow":
				_, err = agent.StartTurn(ctx, owner, "/workspace/job", "thread", "run", input, "model", "high", false)
			case "steer":
				_, err = agent.SteerTurn(ctx, owner, "thread", "turn", "run", input)
			}
			var rejected interface{ DefiniteNoSubmit() bool }
			if !errors.As(err, &rejected) || !rejected.DefiniteNoSubmit() {
				t.Fatalf("unsupported image was not rejected before native work: %v", err)
			}
		})
	}
}
