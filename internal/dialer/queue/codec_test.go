package queue

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// TestScore_PriorityBandOrdering — the core invariant the formula was
// designed around: a fresh priority-N item NEVER beats a stale
// priority-(N-1) item. Tested across the full priority range.
func TestScore_PriorityBandOrdering(t *testing.T) {
	t.Parallel()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	new := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // 5 months later

	for i := range int(maxPriority) {
		// Stale higher-priority (lower number) item must beat a
		// fresh lower-priority (higher number) item.
		p := uint8(i) //nolint:gosec // i is bounded by maxPriority (9)
		stale := score(p, old)
		fresh := score(p+1, new)
		require.Lessf(t, stale, fresh,
			"priority %d at %s must rank above priority %d at %s", p, old, p+1, new)
	}
}

// TestScore_SamePriorityFIFO — within a priority band, the older item
// has the lower score (and therefore pops first via ZPOPMIN).
func TestScore_SamePriorityFIFO(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	older := score(5, at)
	newer := score(5, at.Add(time.Second))
	require.Less(t, older, newer)
}

// TestScore_ClampsAboveMax — a Priority of 250 (above 9) must be clamped
// rather than blowing up the formula. We assert the clamped score equals
// the maxPriority score.
func TestScore_ClampsAboveMax(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	require.InDelta(t, score(maxPriority, at), score(250, at), 0.0)
	require.InDelta(t, score(maxPriority, at), score(255, at), 0.0)
}

// TestScore_FloatPrecisionInRange — the formula stays exact within
// ±2^53 integer precision for our timestamp range. Asserts the round
// trip through float64 -> int64 preserves the ms exactly.
func TestScore_FloatPrecisionInRange(t *testing.T) {
	t.Parallel()
	// Year 2099 timestamp — well below the 2^53 ms limit (~year 285616).
	far := time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC)
	for i := range int(maxPriority) + 1 {
		p := uint8(i) //nolint:gosec // i is bounded by maxPriority+1 (10)
		s := score(p, far)
		expected := float64(p)*1e9 + float64(far.UnixMilli())
		require.InDeltaf(t, expected, s, 0.0,
			"priority=%d at=%s must compute exactly", p, far)
		require.False(t, math.IsInf(s, 0))
		require.False(t, math.IsNaN(s))
	}
}

// TestEncodeItem_DeterministicByteOrder — two items with identical
// logical content produce byte-identical JSON. This is the invariant
// the dedup SET relies on.
func TestEncodeItem_DeterministicByteOrder(t *testing.T) {
	t.Parallel()
	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	at := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)

	item := api.QueueItem{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respID,
		Priority:     5,
		EnqueuedAt:   at,
		AttemptN:     2,
		Phone:        "+79991234567",
		Region:       "RU-MOW",
	}
	a := encodeItem(item)
	b := encodeItem(item)
	require.Equal(t, a, b, "two encodes of the same item must be byte-identical")

	// Field order must follow the documented sequence — TenantID,
	// ProjectID, RespondentID, Priority, EnqueuedAt, AttemptN,
	// Phone, Region. Assert via key positions in the byte string.
	str := string(a)
	tenantPos := strings.Index(str, `"tenant_id"`)
	projectPos := strings.Index(str, `"project_id"`)
	respPos := strings.Index(str, `"respondent_id"`)
	priPos := strings.Index(str, `"priority"`)
	enqPos := strings.Index(str, `"enqueued_at_ms"`)
	attPos := strings.Index(str, `"attempt_n"`)
	phonePos := strings.Index(str, `"phone"`)
	regPos := strings.Index(str, `"region"`)

	require.Less(t, tenantPos, projectPos)
	require.Less(t, projectPos, respPos)
	require.Less(t, respPos, priPos)
	require.Less(t, priPos, enqPos)
	require.Less(t, enqPos, attPos)
	require.Less(t, attPos, phonePos)
	require.Less(t, phonePos, regPos)
}

// TestEncodeDecode_RoundTrip — a wide table of items round-trips
// cleanly. Catches every field-faithful invariant the canonicalised
// JSON has to honour.
func TestEncodeDecode_RoundTrip(t *testing.T) {
	t.Parallel()
	tenantID, projectID, respID := uuid.New(), uuid.New(), uuid.New()
	at := time.Date(2026, 5, 8, 12, 0, 0, 123, time.UTC)

	cases := []struct {
		name string
		in   api.QueueItem
	}{
		{
			name: "fully populated",
			in: api.QueueItem{
				TenantID:     tenantID,
				ProjectID:    projectID,
				RespondentID: respID,
				Priority:     5,
				EnqueuedAt:   at,
				AttemptN:     3,
				Phone:        "+79991234567",
				Region:       "RU-MOW",
			},
		},
		{
			name: "zero priority",
			in: api.QueueItem{
				TenantID:     tenantID,
				ProjectID:    projectID,
				RespondentID: respID,
				Priority:     0,
				EnqueuedAt:   at,
				Phone:        "+79991234567",
				Region:       "RU-SPE",
			},
		},
		{
			name: "max priority",
			in: api.QueueItem{
				TenantID:     tenantID,
				ProjectID:    projectID,
				RespondentID: respID,
				Priority:     maxPriority,
				EnqueuedAt:   at,
				AttemptN:     7,
				Phone:        "+74959999999",
				Region:       "RU-KAM",
			},
		},
		{
			name: "above-max priority gets clamped",
			in: api.QueueItem{
				TenantID:     tenantID,
				ProjectID:    projectID,
				RespondentID: respID,
				Priority:     250,
				EnqueuedAt:   at,
				Phone:        "+79991234567",
				Region:       "RU-MOW",
			},
		},
		{
			name: "phone with quote (JSON escape correctness)",
			in: api.QueueItem{
				TenantID:     tenantID,
				ProjectID:    projectID,
				RespondentID: respID,
				Priority:     5,
				EnqueuedAt:   at,
				Phone:        `+7"weird"`,
				Region:       "RU-MOW",
			},
		},
		{
			name: "empty phone & region",
			in: api.QueueItem{
				TenantID:     tenantID,
				ProjectID:    projectID,
				RespondentID: respID,
				Priority:     5,
				EnqueuedAt:   at,
			},
		},
		{
			name: "epoch ms boundary",
			in: api.QueueItem{
				TenantID:     tenantID,
				ProjectID:    projectID,
				RespondentID: respID,
				Priority:     1,
				EnqueuedAt:   time.UnixMilli(0).UTC(),
				Phone:        "+79991234567",
				Region:       "RU-MOW",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			blob := encodeItem(c.in)

			// Bytes must be valid JSON.
			var sanity map[string]any
			require.NoError(t, json.Unmarshal(blob, &sanity))

			out, err := decodeItem(blob)
			require.NoError(t, err)

			expectedPriority := min(c.in.Priority, maxPriority)
			require.Equal(t, c.in.TenantID, out.TenantID)
			require.Equal(t, c.in.ProjectID, out.ProjectID)
			require.Equal(t, c.in.RespondentID, out.RespondentID)
			require.Equal(t, expectedPriority, out.Priority)
			require.Equal(t, c.in.EnqueuedAt.UTC().UnixMilli(), out.EnqueuedAt.UnixMilli())
			require.Equal(t, c.in.AttemptN, out.AttemptN)
			require.Equal(t, c.in.Phone, out.Phone)
			require.Equal(t, c.in.Region, out.Region)
		})
	}
}

// TestDecodeItem_RejectsCorruptInput — bad JSON / bad UUIDs / wrong
// types all surface a wrapped error naming the failed field.
func TestDecodeItem_RejectsCorruptInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		blob    []byte
		wantSub string
	}{
		{
			name:    "not JSON",
			blob:    []byte("not-json"),
			wantSub: "unmarshal",
		},
		{
			name:    "bad tenant_id uuid",
			blob:    []byte(`{"tenant_id":"not-a-uuid","project_id":"00000000-0000-0000-0000-000000000000","respondent_id":"00000000-0000-0000-0000-000000000000","priority":1,"enqueued_at_ms":0,"attempt_n":0,"phone":"","region":""}`),
			wantSub: "tenant_id",
		},
		{
			name:    "bad project_id uuid",
			blob:    []byte(`{"tenant_id":"00000000-0000-0000-0000-000000000000","project_id":"bad","respondent_id":"00000000-0000-0000-0000-000000000000","priority":1,"enqueued_at_ms":0,"attempt_n":0,"phone":"","region":""}`),
			wantSub: "project_id",
		},
		{
			name:    "bad respondent_id uuid",
			blob:    []byte(`{"tenant_id":"00000000-0000-0000-0000-000000000000","project_id":"00000000-0000-0000-0000-000000000000","respondent_id":"bad","priority":1,"enqueued_at_ms":0,"attempt_n":0,"phone":"","region":""}`),
			wantSub: "respondent_id",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeItem(c.blob)
			require.Error(t, err)
			require.Contains(t, err.Error(), c.wantSub)
		})
	}
}
