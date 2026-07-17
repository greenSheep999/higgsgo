package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// ModelHealthStore implements ports.ModelHealthStore backed by SQLite.
//
// Rows land in the model_health table (see migration 001_init.sql). The
// primary key is (jst, checked_at) so every probe run appends a fresh row
// and Latest resolves via ORDER BY checked_at DESC. This preserves history
// for regressions dashboards and lets List queries in the regression
// ticker pick "oldest last-checked" candidates without a separate join.
type ModelHealthStore struct {
	db *DB
}

// NewModelHealthStore returns a ModelHealthStore rooted at the given DB.
func NewModelHealthStore(db *DB) *ModelHealthStore { return &ModelHealthStore{db: db} }

// Insert appends a single recheck outcome to model_health.
//
// jst is the higgsfield job_set_type (e.g. "seedance_2_0"). verdict is the
// domain.JobStatus produced by the probe (typically completed / failed).
// httpStatus, cost and pollSec are optional metrics; zero values are
// persisted as NULL to distinguish "not measured" from "measured as 0".
func (s *ModelHealthStore) Insert(
	ctx context.Context,
	jst string,
	checkedAt time.Time,
	verdict domain.JobStatus,
	httpStatus int,
	cost int64,
	pollSec int,
) error {
	if jst == "" {
		return errors.New("insert model health: jst is required")
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO model_health (jst, checked_at, verdict, http_status, cost, poll_time_sec)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(jst, checked_at) DO UPDATE SET
			verdict       = excluded.verdict,
			http_status   = excluded.http_status,
			cost          = excluded.cost,
			poll_time_sec = excluded.poll_time_sec`,
		jst,
		fmtTime(checkedAt),
		string(verdict),
		nullInt(int64(httpStatus)),
		nullInt(cost),
		nullInt(int64(pollSec)),
	)
	if err != nil {
		return fmt.Errorf("insert model health %s: %w", jst, err)
	}
	return nil
}

// Latest returns the most recent recheck row for a jst, or (nil, nil) when
// no probes have ever been recorded for that jst. Callers use the nil
// return to distinguish "never checked" from other errors.
func (s *ModelHealthStore) Latest(ctx context.Context, jst string) (*ports.ModelHealthRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT jst, checked_at, verdict, http_status, cost, poll_time_sec
		FROM model_health
		WHERE jst = ?
		ORDER BY checked_at DESC
		LIMIT 1`, jst)
	return scanModelHealth(row)
}

// scanModelHealth reads one model_health row into a ports.ModelHealthRow.
// Returns (nil, nil) when the row set is empty so Latest can distinguish
// "never checked" from actual errors.
func scanModelHealth(sc scanner) (*ports.ModelHealthRow, error) {
	var (
		r          ports.ModelHealthRow
		checkedAt  string
		verdictStr string
		httpStatus sql.NullInt64
		cost       sql.NullInt64
		pollSec    sql.NullInt64
	)
	if err := sc.Scan(&r.JST, &checkedAt, &verdictStr, &httpStatus, &cost, &pollSec); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.CheckedAt = parseTime(checkedAt)
	r.Verdict = domain.JobStatus(verdictStr)
	r.HTTPStatus = int(httpStatus.Int64)
	r.Cost = cost.Int64
	r.PollTimeSec = int(pollSec.Int64)
	return &r, nil
}
