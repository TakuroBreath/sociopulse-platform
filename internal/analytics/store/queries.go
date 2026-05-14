package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
)

// ErrNilQueryConn is returned when one of the *ByMV helpers is called
// with a nil *Conn. The callers (service.QueryService) are expected to
// inject a wired Conn; nil signals a wiring bug.
var ErrNilQueryConn = errors.New("analytics/store: nil conn (query)")

// zeroUUID is the documented sentinel used for the project-optional
// predicate. The AggregatingMergeTree state tables store no zero-UUID
// project rows in v1 (events_calls.project_id is non-Nullable and
// every producer sets it), so an OR(project = sentinel) predicate
// against the zero-UUID literal evaluates as "filter disabled" for
// real data — exactly the semantics the optional filter wants.
var zeroUUID uuid.UUID

// projectFilter returns (value, predicate) for the project-optional
// idiom: when q.ProjectID is nil, value is the zero UUID and the
// predicate matches every row (project_id is non-Nullable in
// events_calls so the OR side fires); when q.ProjectID is set, value
// is the supplied UUID and the predicate restricts to it.
//
// Pass the returned (zero, value) pair into the SQL twice — once for
// the OR(? = zeroUUID) gate, once for the project_id = ? branch.
// Pre-extracting here keeps the call sites readable.
func projectFilter(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return zeroUUID
	}
	return *p
}

// CallsByMV reads aggregated calls counters from mv_calls_hourly for
// (tenant, optional project, window). Returns a CallsResult with zero
// fields when the MV has no rows in the window.
//
// The SELECT uses sumMerge over the AggregatingMergeTree state columns
// — reading state columns directly without *Merge returns binary
// partial-state, not numeric values (see docs/architecture/analytics-mv.md).
//
// The canonical read endpoint is the materialised view `mv_calls_hourly`
// (NOT the underlying `mv_calls_hourly_state` table). Per Plan 13.1
// production lesson #2, two-feeder MVs require a plain VIEW alias for
// the canonical read; mv_calls_hourly is itself the MV-to-state alias
// and is the documented read endpoint.
//
// AvgDurSec is computed by the caller (service.QueryService) from
// TotalDurSec / Total because we want a stable definition of "avg" even
// when the MV has zero rows (avoid divide-by-zero on the wire).
func CallsByMV(ctx context.Context, conn *Conn, q apianalytics.CallsQuery) (apianalytics.CallsResult, error) {
	if conn == nil {
		return apianalytics.CallsResult{}, ErrNilQueryConn
	}

	// Totals query — one row.
	const totalsSQL = `
		SELECT
			sumMerge(cnt)          AS total,
			sumMerge(duration_sec) AS dur
		FROM mv_calls_hourly
		WHERE tenant_id = ?
		  AND bucket_hour >= ? AND bucket_hour < ?
		  AND (? = toUUID('00000000-0000-0000-0000-000000000000') OR project_id = ?)`

	pf := projectFilter(q.ProjectID)
	var totals struct {
		Total uint64
		Dur   uint64
	}
	if err := conn.Driver().QueryRow(ctx, totalsSQL,
		q.TenantID, q.Window.From, q.Window.To, pf, pf,
	).Scan(&totals.Total, &totals.Dur); err != nil {
		return apianalytics.CallsResult{}, fmt.Errorf("analytics/store: query calls totals: %w", err)
	}

	// Per-status breakdown — used to fill Successful/Failed/Refusals + ByStatus.
	const byStatusSQL = `
		SELECT
			status,
			sumMerge(cnt) AS n
		FROM mv_calls_hourly
		WHERE tenant_id = ?
		  AND bucket_hour >= ? AND bucket_hour < ?
		  AND (? = toUUID('00000000-0000-0000-0000-000000000000') OR project_id = ?)
		GROUP BY status`

	rows, err := conn.Driver().Query(ctx, byStatusSQL,
		q.TenantID, q.Window.From, q.Window.To, pf, pf,
	)
	if err != nil {
		return apianalytics.CallsResult{}, fmt.Errorf("analytics/store: query calls by_status: %w", err)
	}
	defer rows.Close()

	res := apianalytics.CallsResult{
		Total:       totals.Total,
		TotalDurSec: totals.Dur,
	}
	for rows.Next() {
		var status string
		var n uint64
		if err := rows.Scan(&status, &n); err != nil {
			return apianalytics.CallsResult{}, fmt.Errorf("analytics/store: scan calls by_status: %w", err)
		}
		res.ByStatus = append(res.ByStatus, apianalytics.StatusBucket{Status: status, Count: n})
		switch status {
		case "success":
			res.Successful = n
		case "fail":
			res.Failed = n
		case "refusal":
			res.Refusals = n
		}
	}
	if err := rows.Err(); err != nil {
		return apianalytics.CallsResult{}, fmt.Errorf("analytics/store: iterate calls by_status: %w", err)
	}

	if res.Total > 0 {
		res.AvgDurSec = float64(res.TotalDurSec) / float64(res.Total)
	}

	return res, nil
}

// OperatorStateByMV reads time-in-state aggregates from
// mv_operator_kpi_daily for (tenant, optional operator, optional
// project, window). Returns zero-valued OperatorStateBreakdown when no
// rows match the filter.
//
// The MV is a two-feeder AggregatingMergeTree (Plan 13.1 § two-feeder
// MV pattern): `mv_operator_kpi_daily_calls` writes call counters
// while `mv_operator_kpi_daily_states` writes duration counters. The
// canonical read endpoint is the plain VIEW `mv_operator_kpi_daily`
// (see migrations/clickhouse/000005). Reading either feeder directly
// returns zeroed columns for the dimensions it doesn't own.
func OperatorStateByMV(ctx context.Context, conn *Conn, q apianalytics.OperatorStateQuery) (apianalytics.OperatorStateBreakdown, error) {
	if conn == nil {
		return apianalytics.OperatorStateBreakdown{}, ErrNilQueryConn
	}

	const stmt = `
		SELECT
			sumMerge(talk_sec)  AS talk,
			sumMerge(pause_sec) AS pause,
			sumMerge(ready_sec) AS ready,
			sumMerge(wrap_sec)  AS wrap
		FROM mv_operator_kpi_daily
		WHERE tenant_id = ?
		  AND bucket_date >= toDate(?) AND bucket_date < toDate(?)
		  AND (? = toUUID('00000000-0000-0000-0000-000000000000') OR user_id    = ?)
		  AND (? = toUUID('00000000-0000-0000-0000-000000000000') OR project_id = ?)`

	opFilter := projectFilter(q.OperatorID)
	pf := projectFilter(q.ProjectID)

	var b apianalytics.OperatorStateBreakdown
	if err := conn.Driver().QueryRow(ctx, stmt,
		q.TenantID,
		q.Window.From, q.Window.To,
		opFilter, opFilter,
		pf, pf,
	).Scan(&b.TalkSec, &b.PauseSec, &b.ReadySec, &b.WrapSec); err != nil {
		return apianalytics.OperatorStateBreakdown{}, fmt.Errorf("analytics/store: query operator_state: %w", err)
	}
	return b, nil
}

// RegionProgressDoneByMV reads per-region done-call counts from
// mv_quotas_progress for (tenant, project, window). Returns a map of
// region_code → done. The Plan (quota target) is supplied separately
// by service.QueryService via the crm port (Q12).
//
// "Done" here is defined as success_cnt — fails / refusals are NOT
// counted toward quota completion (matches the master spec's quota
// semantics: only a successful interview consumes a quota slot).
func RegionProgressDoneByMV(ctx context.Context, conn *Conn, q apianalytics.RegionProgressQuery) (map[string]uint64, error) {
	if conn == nil {
		return nil, ErrNilQueryConn
	}

	const stmt = `
		SELECT
			region_code,
			sumMerge(success_cnt) AS done
		FROM mv_quotas_progress
		WHERE tenant_id = ?
		  AND project_id = ?
		  AND bucket_date >= toDate(?) AND bucket_date < toDate(?)
		GROUP BY region_code`

	rows, err := conn.Driver().Query(ctx, stmt,
		q.TenantID, q.ProjectID, q.Window.From, q.Window.To,
	)
	if err != nil {
		return nil, fmt.Errorf("analytics/store: query region_progress: %w", err)
	}
	defer rows.Close()

	out := make(map[string]uint64)
	for rows.Next() {
		var region string
		var done uint64
		if err := rows.Scan(&region, &done); err != nil {
			return nil, fmt.Errorf("analytics/store: scan region_progress: %w", err)
		}
		out[region] = done
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics/store: iterate region_progress: %w", err)
	}
	return out, nil
}

// HourlyByMV reads per-hour activity buckets from mv_calls_hourly for
// (tenant, optional project, window). Each row is one HourlyBucket
// {Hour, Count, AvgDurSec}.
//
// Buckets are emitted in ascending Hour order so consumers (typically
// dashboard line charts) need not re-sort. AvgDurSec is computed as
// the if(cnt=0, 0, dur/cnt) inside CH to avoid divide-by-zero +
// surface a stable zero when a bucket has no rows.
func HourlyByMV(ctx context.Context, conn *Conn, q apianalytics.HourlyQuery) ([]apianalytics.HourlyBucket, error) {
	if conn == nil {
		return nil, ErrNilQueryConn
	}

	const stmt = `
		SELECT
			bucket_hour                                                            AS hour,
			sumMerge(cnt)                                                          AS total,
			sumMerge(duration_sec)                                                 AS dur
		FROM mv_calls_hourly
		WHERE tenant_id = ?
		  AND bucket_hour >= ? AND bucket_hour < ?
		  AND (? = toUUID('00000000-0000-0000-0000-000000000000') OR project_id = ?)
		GROUP BY bucket_hour
		ORDER BY bucket_hour`

	pf := projectFilter(q.ProjectID)
	rows, err := conn.Driver().Query(ctx, stmt,
		q.TenantID, q.Window.From, q.Window.To, pf, pf,
	)
	if err != nil {
		return nil, fmt.Errorf("analytics/store: query hourly: %w", err)
	}
	defer rows.Close()

	var out []apianalytics.HourlyBucket
	for rows.Next() {
		var b apianalytics.HourlyBucket
		var total, dur uint64
		if err := rows.Scan(&b.Hour, &total, &dur); err != nil {
			return nil, fmt.Errorf("analytics/store: scan hourly: %w", err)
		}
		b.Count = total
		if total > 0 {
			b.AvgDurSec = float64(dur) / float64(total)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics/store: iterate hourly: %w", err)
	}
	return out, nil
}

// OperatorComparisonsByMV reads per-operator aggregates from
// mv_operator_kpi_daily for (tenant, project, window). Returns one row
// per (operator_id) with CallsTotal, SuccessRate, AvgTalkSec, PauseShare.
//
// The store leaves DisplayName empty and AboveTeamAvg false; both are
// derived by the caller (service.QueryService) — DisplayName comes
// from a user resolver port (not yet wired in v1; TODO documented),
// AboveTeamAvg is computed AFTER the team-average is known across the
// returned set.
func OperatorComparisonsByMV(ctx context.Context, conn *Conn, q apianalytics.OperatorComparisonsQuery) ([]apianalytics.OperatorComparisonRow, error) {
	if conn == nil {
		return nil, ErrNilQueryConn
	}

	const stmt = `
		SELECT
			user_id,
			sumMerge(calls_total)   AS total,
			sumMerge(calls_success) AS success,
			sumMerge(talk_sec)      AS talk,
			sumMerge(pause_sec)     AS pause
		FROM mv_operator_kpi_daily
		WHERE tenant_id = ?
		  AND project_id = ?
		  AND bucket_date >= toDate(?) AND bucket_date < toDate(?)
		GROUP BY user_id`

	rows, err := conn.Driver().Query(ctx, stmt,
		q.TenantID, q.ProjectID, q.Window.From, q.Window.To,
	)
	if err != nil {
		return nil, fmt.Errorf("analytics/store: query operator_comparisons: %w", err)
	}
	defer rows.Close()

	var out []apianalytics.OperatorComparisonRow
	for rows.Next() {
		var (
			op                          uuid.UUID
			total, success, talk, pause uint64
		)
		if err := rows.Scan(&op, &total, &success, &talk, &pause); err != nil {
			return nil, fmt.Errorf("analytics/store: scan operator_comparisons: %w", err)
		}
		row := apianalytics.OperatorComparisonRow{
			OperatorID: op,
			CallsTotal: total,
			// DisplayName intentionally empty in v1 — see docstring.
			// AboveTeamAvg filled by service.QueryService after team-avg known.
		}
		if total > 0 {
			row.SuccessRate = float64(success) / float64(total)
			row.AvgTalkSec = float64(talk) / float64(total)
		}
		denom := talk + pause
		if denom > 0 {
			row.PauseShare = float64(pause) / float64(denom)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics/store: iterate operator_comparisons: %w", err)
	}
	return out, nil
}
