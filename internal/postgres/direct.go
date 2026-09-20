package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
)

func (s Store) AdmitDirect(ctx context.Context, input core.SessionAdmission, queueName string) (core.Session, bool, error) {
	normalized, err := normalizeCoreAdmission(input)
	if err != nil {
		return core.Session{}, false, err
	}
	session, created, err := admitSession(ctx, s, normalized, queueName, direct.TaskName, direct.TaskKey)
	if errors.Is(err, ErrAdmissionConflict) {
		err = fmt.Errorf("%w: %w", direct.ErrAdmissionConflict, err)
	}
	return session, created, err
}
