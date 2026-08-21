package absurdruntime

import (
	"errors"
	"io"
	"log"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	taskExecutionFailedFormat = "[absurd] task execution failed: %v"
	taskAttemptFailedFormat   = "[absurd] task attempt failed: %v"
	taskEventWaitTimedOut     = "[absurd] task attempt %v"
)

// WorkerLogger preserves Absurd's logs while making its run-level failure
// message distinguish one task attempt from the durable Task.
func WorkerLogger(output io.Writer) absurd.Logger {
	return workerLogger{Logger: log.New(output, "", log.LstdFlags)}
}

type workerLogger struct {
	*log.Logger
}

func (l workerLogger) Printf(format string, args ...any) {
	if format != taskExecutionFailedFormat || len(args) != 1 {
		l.Logger.Printf(format, args...)
		return
	}

	format = taskAttemptFailedFormat
	if err, ok := args[0].(error); ok {
		var timeout *absurd.TimeoutError
		if errors.As(err, &timeout) {
			format = taskEventWaitTimedOut
		}
	}
	l.Logger.Printf(format, args...)
}
