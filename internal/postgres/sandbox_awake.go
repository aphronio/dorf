package postgres

import "context"

// WithSandboxAwake prevents idle suspension while a bounded operation uses the
// guest. Shared locks permit ordinary Session execution and concurrent readers;
// they do not claim the Session mutation fence or certify checkpoint consistency.
func (s Store) WithSandboxAwake(ctx context.Context, sessionID string, operation func() error) error {
	return s.withSandboxPowerFence(ctx, sessionID, `select true from pg_advisory_xact_lock_shared(hashtextextended('dorf-sandbox-power:' || $1, 0))`, operation)
}

// WithSandboxPauseFence skips optional suspension if the guest is in use. The
// caller retains its ordinary Session fence through the provider pause operation.
func (s Store) WithSandboxPauseFence(ctx context.Context, sessionID string, operation func() error) error {
	return s.withSandboxPowerFence(ctx, sessionID, `select pg_try_advisory_xact_lock(hashtextextended('dorf-sandbox-power:' || $1, 0))`, operation)
}

func (s Store) withSandboxPowerFence(ctx context.Context, sessionID, query string, operation func() error) error {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// As with the Session fence, cancellation must not release the lock before
	// the caller has finished stopping its remote commands and cleaning up.
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var acquired bool
	if err := tx.QueryRowContext(ctx, query, sessionID).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	if err := operation(); err != nil {
		return err
	}
	return tx.Commit()
}
