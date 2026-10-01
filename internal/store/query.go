package store

import (
	"context"
	"database/sql"
	"strings"
)

// Agg is one aggregated SQL row: all four dimensions plus an optional time
// bucket (start ms, UTC). Cost sums priced rows only.
type Agg struct {
	BucketMS                                     int64
	Model, Provider, Credential, Client          string
	Requests, Failed, Unpriced                   int64
	TInput, TCacheRead, TCacheWrite, TOutput, TR int64
	Cost                                         float64
}

// Aggregate groups raw rows (fromRaw) or daily rollups in [sinceMS, untilMS).
// bucketMS > 0 adds a time bucket of that width (raw) — rollups always bucket
// by UTC day when bucketMS > 0.
func (s *Store) Aggregate(ctx context.Context, sinceMS, untilMS int64, fromRaw bool, bucketMS int64) ([]Agg, error) {
	var q string
	var args []any
	if fromRaw {
		bucket := "0"
		if bucketMS > 0 {
			bucket = "(requested_at_ms / ?) * ?"
			args = append(args, bucketMS, bucketMS)
		}
		q = `SELECT ` + bucket + ` AS b, model, provider, COALESCE(credential,''), COALESCE(client,''),
COUNT(*), SUM(failed), SUM(c_total IS NULL), SUM(t_input), SUM(t_cache_read), SUM(t_cache_write), SUM(t_output), SUM(t_reasoning),
COALESCE(SUM(c_total),0)
FROM requests WHERE requested_at_ms >= ? AND requested_at_ms < ?
GROUP BY b, model, provider, COALESCE(credential,''), COALESCE(client,'')`
		args = append(args, sinceMS, untilMS)
	} else {
		bucket := "0"
		if bucketMS > 0 {
			bucket = "CAST(unixepoch(day) AS INTEGER) * 1000"
		}
		q = `SELECT ` + bucket + ` AS b, model, provider, credential, client,
SUM(requests), SUM(failed), SUM(unpriced), SUM(t_input), SUM(t_cache_read), SUM(t_cache_write), SUM(t_output), SUM(t_reasoning), SUM(c_total)
FROM daily_rollups WHERE day >= ? AND day <= ?
GROUP BY b, model, provider, credential, client`
		args = append(args, dayUTC(sinceMS), dayUTC(untilMS-1))
	}
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agg
	for rows.Next() {
		var a Agg
		if err := rows.Scan(&a.BucketMS, &a.Model, &a.Provider, &a.Credential, &a.Client, &a.Requests, &a.Failed, &a.Unpriced,
			&a.TInput, &a.TCacheRead, &a.TCacheWrite, &a.TOutput, &a.TR, &a.Cost); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Sample is one raw row's latency data for percentiles.
type Sample struct {
	AtMS                                int64
	Model, Provider, Credential, Client string
	LatencyMS, TTFTMS                   *float64
}

// Samples returns up to limit raw latency samples in range; truncated reports
// that more rows exist.
func (s *Store) Samples(ctx context.Context, sinceMS, untilMS int64, limit int) ([]Sample, bool, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT requested_at_ms, model, provider, COALESCE(credential,''), COALESCE(client,''), latency_ms, ttft_ms
FROM requests WHERE requested_at_ms >= ? AND requested_at_ms < ? LIMIT ?`, sinceMS, untilMS, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]Sample, 0, 256)
	for rows.Next() {
		if len(out) == limit {
			return out, true, nil
		}
		var smp Sample
		if err := rows.Scan(&smp.AtMS, &smp.Model, &smp.Provider, &smp.Credential, &smp.Client, &smp.LatencyMS, &smp.TTFTMS); err != nil {
			return nil, false, err
		}
		out = append(out, smp)
	}
	return out, false, rows.Err()
}

// UnpricedModel is a model with unpriced requests in range.
type UnpricedModel struct {
	Model, Provider, Status string
	Requests                int64
}

// Unpriced lists models with unpriced rows. Raw rows carry the pricing
// status; rollups only know "unpriced" and report status "unknown".
func (s *Store) Unpriced(ctx context.Context, sinceMS, untilMS int64, fromRaw bool) ([]UnpricedModel, error) {
	var rows *sql.Rows
	var err error
	if fromRaw {
		rows, err = s.r.QueryContext(ctx, `SELECT model, provider, pricing_status, COUNT(*) FROM requests
WHERE requested_at_ms >= ? AND requested_at_ms < ? AND c_total IS NULL
GROUP BY model, provider, pricing_status ORDER BY COUNT(*) DESC, model`, sinceMS, untilMS)
	} else {
		rows, err = s.r.QueryContext(ctx, `SELECT model, provider, 'unknown', SUM(unpriced) FROM daily_rollups
WHERE day >= ? AND day <= ? AND unpriced > 0 GROUP BY model, provider ORDER BY SUM(unpriced) DESC, model`, dayUTC(sinceMS), dayUTC(untilMS-1))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnpricedModel
	for rows.Next() {
		var u UnpricedModel
		if err := rows.Scan(&u.Model, &u.Provider, &u.Status, &u.Requests); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TokenMismatches counts raw rows flagged token_mismatch in range.
func (s *Store) TokenMismatches(ctx context.Context, sinceMS, untilMS int64) (int64, error) {
	var n int64
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE requested_at_ms >= ? AND requested_at_ms < ? AND token_mismatch = 1`, sinceMS, untilMS).Scan(&n)
	return n, err
}

const attemptCols = `request_id, trace_id, requested_at_ms, provider, model, COALESCE(response_model,''), COALESCE(auth_id,''),
COALESCE(credential,''), client, stream, failed, failure_status, latency_ms, ttft_ms,
t_input, t_cache_read, t_cache_write, t_output, t_reasoning,
c_total, c_input, c_cache_read, c_cache_write, c_output, pricing_status, rate_card_id, tier_above`

func scanAttempts(rows *sql.Rows) ([]Row, error) {
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var stream, failed int64
		if err := rows.Scan(&r.RequestID, &r.TraceID, &r.RequestedAtMS, &r.Provider, &r.Model, &r.ResponseModel, &r.AuthID,
			&r.Credential, &r.Client, &stream, &failed, &r.FailureStatus, &r.LatencyMS, &r.TTFTMS,
			&r.TInput, &r.TCacheRead, &r.TCacheWrite, &r.TOutput, &r.TReasoning,
			&r.CTotal, &r.CInput, &r.CCacheRead, &r.CCacheWrite, &r.COutput, &r.PricingStatus, &r.RateCardID, &r.TierAbove); err != nil {
			return nil, err
		}
		r.Stream, r.Failed = stream == 1, failed == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ByTrace returns attempts for the given trace ids, oldest first.
func (s *Store) ByTrace(ctx context.Context, ids []string) ([]Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.r.QueryContext(ctx, `SELECT `+attemptCols+` FROM requests WHERE trace_id IN (`+placeholders(len(ids))+`) ORDER BY requested_at_ms, request_id`, args...)
	if err != nil {
		return nil, err
	}
	return scanAttempts(rows)
}

// ByRequest returns the attempt with the given request id, if any.
func (s *Store) ByRequest(ctx context.Context, id string) ([]Row, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+attemptCols+` FROM requests WHERE request_id = ?`, id)
	if err != nil {
		return nil, err
	}
	return scanAttempts(rows)
}

// Recent returns up to n attempts requested before beforeMS, newest first;
// failedOnly restricts it to failed attempts.
func (s *Store) Recent(ctx context.Context, beforeMS int64, n int, failedOnly bool) ([]Row, error) {
	q := `SELECT ` + attemptCols + ` FROM requests WHERE requested_at_ms < ? ORDER BY requested_at_ms DESC, request_id DESC LIMIT ?`
	if failedOnly {
		q = `SELECT ` + attemptCols + ` FROM requests WHERE failed = 1 AND requested_at_ms < ? ORDER BY requested_at_ms DESC, request_id DESC LIMIT ?`
	}
	rows, err := s.r.QueryContext(ctx, q, beforeMS, n)
	if err != nil {
		return nil, err
	}
	return scanAttempts(rows)
}

// Top returns up to n priced attempts in [sinceMS, untilMS), most expensive first.
func (s *Store) Top(ctx context.Context, sinceMS, untilMS int64, n int) ([]Row, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+attemptCols+` FROM requests
WHERE requested_at_ms >= ? AND requested_at_ms < ? AND c_total IS NOT NULL
ORDER BY c_total DESC, requested_at_ms DESC, request_id DESC LIMIT ?`, sinceMS, untilMS, n)
	if err != nil {
		return nil, err
	}
	return scanAttempts(rows)
}

// CacheAgg sums the cache buckets of priced raw rows per dimension, rate card and tier.
type CacheAgg struct {
	Model, Provider, Credential, Client, RateCardID string
	TierAbove                                       int64 // -1 = base rates
	TCacheRead, TCacheWrite                         int64
	CCacheRead, CCacheWrite                         float64
}

// CacheBasis returns the cache token and cost sums of priced raw rows in range.
func (s *Store) CacheBasis(ctx context.Context, sinceMS, untilMS int64) ([]CacheAgg, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT model, provider, COALESCE(credential,''), COALESCE(client,''), rate_card_id, COALESCE(tier_above,-1),
  SUM(t_cache_read), SUM(t_cache_write), SUM(COALESCE(c_cache_read,0)), SUM(COALESCE(c_cache_write,0))
FROM requests
WHERE requested_at_ms >= ? AND requested_at_ms < ? AND c_total IS NOT NULL AND rate_card_id IS NOT NULL
GROUP BY model, provider, COALESCE(credential,''), COALESCE(client,''), rate_card_id, COALESCE(tier_above,-1)`, sinceMS, untilMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheAgg
	for rows.Next() {
		var a CacheAgg
		if err := rows.Scan(&a.Model, &a.Provider, &a.Credential, &a.Client, &a.RateCardID, &a.TierAbove,
			&a.TCacheRead, &a.TCacheWrite, &a.CCacheRead, &a.CCacheWrite); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RateCards returns the stored canonical JSON of the given rate cards by id.
func (s *Store) RateCards(ctx context.Context, ids []string) (map[string][]byte, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id, card_json FROM rate_cards WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]byte, len(ids))
	for rows.Next() {
		var id string
		var card []byte
		if err := rows.Scan(&id, &card); err != nil {
			return nil, err
		}
		out[id] = card
	}
	return out, rows.Err()
}

// FailureCount is the number of failed raw rows with one upstream status (nil = none recorded).
type FailureCount struct {
	Status   *int64
	Requests int64
}

// FailureStatuses counts failed raw rows in range by upstream status, most frequent first.
func (s *Store) FailureStatuses(ctx context.Context, sinceMS, untilMS int64) ([]FailureCount, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT failure_status, COUNT(*) FROM requests
WHERE failed = 1 AND requested_at_ms >= ? AND requested_at_ms < ?
GROUP BY failure_status
ORDER BY COUNT(*) DESC, failure_status IS NULL, failure_status`, sinceMS, untilMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailureCount
	for rows.Next() {
		var f FailureCount
		if err := rows.Scan(&f.Status, &f.Requests); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Quotas returns every stored quota snapshot.
func (s *Store) Quotas(ctx context.Context) ([]QuotaObs, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT credential, COALESCE(auth_id,''), provider, observed_at_ms, snapshot_json FROM quota_snapshots ORDER BY provider, credential`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaObs
	for rows.Next() {
		var q QuotaObs
		var js string
		if err := rows.Scan(&q.Credential, &q.AuthID, &q.Provider, &q.ObservedAtMS, &js); err != nil {
			return nil, err
		}
		q.SnapshotJSON = []byte(js)
		out = append(out, q)
	}
	return out, rows.Err()
}

// Models returns every model id ever recorded (rollups outlive raw rows).
func (s *Store) Models(ctx context.Context) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT DISTINCT model FROM daily_rollups ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
