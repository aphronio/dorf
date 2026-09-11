package sandbox

import (
	"fmt"
	"strings"
	"time"
)

const (
	MaxCommandBytes         = 64 << 10
	MaxCommandOutputBytes   = 64 << 10
	MaxCommandSeconds       = 120
	CommandTransportTimeout = (MaxCommandSeconds + 5) * time.Second
)

const (
	MaxCommandRequestBytes  = 6*MaxCommandBytes + 4096
	MaxCommandResponseBytes = 12*MaxCommandOutputBytes + 1024
)

type Command struct {
	Argv           []string `json:"argv"`
	Stdin          string   `json:"stdin,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

func (c Command) Validate() error {
	if len(c.Argv) == 0 || len(c.Argv) > 256 || c.Argv[0] == "" || c.TimeoutSeconds < 0 || c.TimeoutSeconds > MaxCommandSeconds {
		return fmt.Errorf("invalid Sandbox command")
	}
	size := len(c.Stdin)
	for _, arg := range c.Argv {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("Sandbox command argument contains NUL")
		}
		size += len(arg)
	}
	if size > MaxCommandBytes {
		return fmt.Errorf("Sandbox command exceeds input limit")
	}
	return nil
}

func (c Command) Timeout() time.Duration {
	if c.TimeoutSeconds == 0 {
		return 30 * time.Second
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

type CommandResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
}
