package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
)

// beginWrite shortens SQLite's busy wait to the caller's deadline. SQLite's
// busy handler is otherwise allowed to consume the full configured timeout
// before database/sql can observe context cancellation.
func (s *Store) beginWrite(ctx context.Context) (*sql.Tx, func(), error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	release := func() {
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS))
		_ = conn.Close()
	}
	for {
		if err := setContextBusyTimeout(ctx, conn); err != nil {
			release()
			return nil, nil, err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err == nil {
			return tx, release, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			release()
			return nil, nil, ctxErr
		}
		if !isSQLiteBusy(err) {
			release()
			return nil, nil, err
		}
		if _, limited := contextBusyTimeout(ctx); !limited {
			release()
			return nil, nil, err
		}
		// A sub-millisecond remainder can make SQLite report BUSY just before
		// the context timer fires. Wait for the authoritative context result.
		select {
		case <-ctx.Done():
			release()
			return nil, nil, ctx.Err()
		default:
		}
	}
}

func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xff
	return code == 5 || code == 6 // SQLITE_BUSY or SQLITE_LOCKED.
}

func setContextBusyTimeout(ctx context.Context, conn *sql.Conn) error {
	timeout, _ := contextBusyTimeout(ctx)
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", timeout)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("configure SQLite busy timeout: %w", err)
	}
	return nil
}

func contextBusyTimeout(ctx context.Context) (milliseconds int64, limited bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return busyTimeoutMS, false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 1, true
	}
	// Round up so a BUSY result does not normally precede the deadline.
	milliseconds = (remaining.Nanoseconds() + int64(time.Millisecond) - 1) / int64(time.Millisecond)
	if milliseconds < busyTimeoutMS {
		return milliseconds, true
	}
	return busyTimeoutMS, false
}
