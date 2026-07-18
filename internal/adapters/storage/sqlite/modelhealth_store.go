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

// List returns every row in model_health, newest probe first
// (ORDER BY checked_at DESC). The table is bounded by the model
// catalog size, so pagination is intentionally omitted — admin
// surfaces consume it in one shot.
func (s *ModelHealthStore) List(ctx context.Context) ([]ports.ModelHealthRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT jst, checked_at, verdict, http_status, cost, poll_time_sec
		FROM model_health
		ORDER BY checked_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list model health: %w", err)
	}
	defer rows.Close()
	var out []ports.ModelHealthRow
	for rows.Next() {
		r, err := scanModelHealth(rows)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, rows.Err()
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

// SlotsByJST buckets probes for one JST into fixed-width time slots
// so the WebUI's uptime bar can render a per-slot pass/fail history
// instead of the old fabricated placeholder (ROADMAP P3-13).
//
// slotSeconds picks the bucket width (typically 3600 for 1h slots →
// 24h view, or 86400 for 1d slots → 48d view — matches
// generateEmptySlots on the frontend). count is the number of most-
// recent slots to return; missing slots (no probes in that window)
// are returned as total=0 so the bar keeps a stable width.
//
// Returned slots are oldest-first, so the frontend can iterate and
// place each block left-to-right without an extra reverse.
func (s *ModelHealthStore) SlotsByJST(ctx context.Context, jst string, count int, slotSeconds int) ([]ports.HealthSlot, error) {
	if count <= 0 || slotSeconds <= 0 {
		return nil, nil
	}
	// SQLite lacks a native "bucket a timestamp into N-second slots"
	// primitive, so we do the arithmetic in Go: for each of the count
	// most-recent slots, compute [floor(now/slotSec) - i] and query
	// the row aggregate for that window. One round-trip per slot is
	// acceptable — count is small (12 or 48) and the model_health
	// index (jst, checked_at DESC) makes each query O(log n) + the
	// window size.
	//
	// An alternative is one big query grouping by
	// (unixepoch(checked_at) / slotSeconds), then filling gaps in
	// Go. Both work; the per-slot loop keeps the code readable and
	// the fill-gaps logic implicit.
	now := time.Now().UTC()
	// Align current slot to the top so consecutive requests within the
	// same slot return an identical last bucket rather than a sliding
	// window.
	curSlot := now.Unix() / int64(slotSeconds)
	out := make([]ports.HealthSlot, 0, count)
	for i := count - 1; i >= 0; i-- {
		slotStart := time.Unix((curSlot-int64(i))*int64(slotSeconds), 0).UTC()
		slotEnd := slotStart.Add(time.Duration(slotSeconds) * time.Second)

		var total, passed int64
		// COALESCE the SUM because SQLite returns NULL for SUM over an
		// empty set, and QueryRow.Scan can't unpack NULL into int64.
		// COUNT is fine (returns 0), but SUM needs the guard.
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) AS total,
			       COALESCE(SUM(CASE WHEN verdict = 'completed' THEN 1 ELSE 0 END), 0) AS passed
			FROM model_health
			WHERE jst = ?
			  AND checked_at >= ?
			  AND checked_at <  ?`,
			jst,
			fmtTime(slotStart),
			fmtTime(slotEnd),
		).Scan(&total, &passed)
		if err != nil {
			return nil, fmt.Errorf("slots by jst=%s slot=%s: %w", jst, slotStart.Format(time.RFC3339), err)
		}
		out = append(out, ports.HealthSlot{
			Time:   slotStart,
			Total:  int(total),
			Passed: int(passed),
		})
	}
	return out, nil
}

// UptimeByJST computes per-jst uptime as a percentage over all probes
// whose checked_at >= since. A verdict of "completed" counts as success.
func (s *ModelHealthStore) UptimeByJST(ctx context.Context, since time.Time) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT jst,
			COUNT(*) AS total,
			SUM(CASE WHEN verdict = 'completed' THEN 1 ELSE 0 END) AS ok
		FROM model_health
		WHERE checked_at >= ?
		GROUP BY jst`, fmtTime(since.UTC()))
	if err != nil {
		return nil, fmt.Errorf("uptime by jst: %w", err)
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var jst string
		var total, ok int64
		if err := rows.Scan(&jst, &total, &ok); err != nil {
			return nil, err
		}
		if total > 0 {
			out[jst] = float64(ok) / float64(total) * 100.0
		}
	}
	return out, rows.Err()
}
