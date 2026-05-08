package queue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// maxPriority is the inclusive upper bound for the priority field. Anything
// above is clamped on encode (and explicitly capped on Requeue per the API
// contract). The cap is the bound that keeps priority*1e9 + epoch_ms safely
// inside the ±2^53 integer-precision band of a float64.
const maxPriority uint8 = 9

// score returns the ZSET score for a (priority, enqueuedAt) pair. The
// formula intentionally segregates priority bands into 1e9-millisecond
// intervals (~11.5 days) so a fresh priority-5 item never beats a stale
// priority-4 item.
//
// Priority is clamped to maxPriority before the multiplication; passing 250
// would otherwise overflow the float arithmetic and break ordering.
func score(priority uint8, at time.Time) float64 {
	if priority > maxPriority {
		priority = maxPriority
	}
	return float64(priority)*1e9 + float64(at.UnixMilli())
}

// encodeItem serialises a QueueItem to its canonical JSON byte form. The
// fields appear in a fixed order — TenantID, ProjectID, RespondentID,
// Priority, EnqueuedAt (unix ms), AttemptN, Phone, Region — so two items
// with the same logical content produce byte-identical JSON. The ZSET
// uses the resulting bytes verbatim as the member key; an unstable order
// would break the dedup invariant on edges that bypass the dedup SET
// (e.g. Requeue races a manual re-enqueue).
//
// EnqueuedAt is encoded as a unix millisecond integer, NOT RFC3339Nano.
// This keeps the JSON compact and matches the score formula exactly so
// the score round-trips without rounding through a string representation.
//
// The function uses a hand-rolled writer rather than encoding/json on a
// struct so no future field-order shuffle, anonymous-struct rename, or
// stdlib internal change can silently desync the byte layout from the
// dedup invariant. Each string value is JSON-escaped via the stdlib so
// the Lua cjson.decode path round-trips cleanly even when Phone / Region
// contain quotes or backslashes.
//
// Today every field is a UUID, an integer, or a string, none of which
// can fail json.Marshal (the stdlib documents that). encodeItem therefore
// returns the bytes directly — no error pathway. If a future field type
// can fail to marshal (e.g. a custom struct), reintroduce the error
// return and propagate the failure.
func encodeItem(it api.QueueItem) []byte {
	priority := it.Priority
	if priority > maxPriority {
		priority = maxPriority
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	writeJSONStringField(&buf, "tenant_id", it.TenantID.String(), false)
	writeJSONStringField(&buf, "project_id", it.ProjectID.String(), true)
	writeJSONStringField(&buf, "respondent_id", it.RespondentID.String(), true)
	writeJSONNumberField(&buf, "priority", uint64(priority), true)
	writeJSONNumberField(&buf, "enqueued_at_ms", uint64(it.EnqueuedAt.UnixMilli()), true) //nolint:gosec // unix ms positive in our timestamp range
	writeJSONNumberField(&buf, "attempt_n", uint64(it.AttemptN), true)
	writeJSONStringField(&buf, "phone", it.Phone, true)
	writeJSONStringField(&buf, "region", it.Region, true)
	buf.WriteByte('}')
	return buf.Bytes()
}

// writeJSONStringField appends `,"<key>":"<value>"` (or without the
// leading comma when notFirst==false) to buf. The value is JSON-escaped
// via json.Marshal on a string so embedded quotes / control characters
// do not corrupt the JSON. json.Marshal on a string never returns an
// error (the stdlib documents this), so we drop the err return.
func writeJSONStringField(buf *bytes.Buffer, key, value string, notFirst bool) {
	if notFirst {
		buf.WriteByte(',')
	}
	writeJSONKey(buf, key)
	encoded, _ := json.Marshal(value) //nolint:errchkjson // string Marshal never fails (stdlib docs)
	buf.Write(encoded)
}

// writeJSONNumberField is the integer-typed counterpart of
// writeJSONStringField. It writes the value as a bare JSON number (no
// quotes), which is what the Lua cjson decoder expects for typed
// fields like priority / attempt_n / enqueued_at_ms.
func writeJSONNumberField(buf *bytes.Buffer, key string, value uint64, notFirst bool) {
	if notFirst {
		buf.WriteByte(',')
	}
	writeJSONKey(buf, key)
	buf.WriteString(strconv.FormatUint(value, 10))
}

// writeJSONKey appends `"<key>":` to buf. Centralised here so the key
// quoting is consistent across writeJSONStringField and
// writeJSONNumberField (and so a future change to support
// unicode-escaped keys lands in one place). Like the value form, the
// stdlib guarantees json.Marshal on a string does not fail.
func writeJSONKey(buf *bytes.Buffer, key string) {
	encoded, _ := json.Marshal(key) //nolint:errchkjson // string Marshal never fails (stdlib docs)
	buf.Write(encoded)
	buf.WriteByte(':')
}

// queueItemPayload is the wire shape decoded from JSON. It mirrors the
// encodeItem layout — tenant_id, project_id, respondent_id, priority,
// enqueued_at_ms, attempt_n, phone, region — but uses standard
// encoding/json struct tags because decode does not have to be
// byte-deterministic, only field-faithful.
type queueItemPayload struct {
	TenantID     string `json:"tenant_id"`
	ProjectID    string `json:"project_id"`
	RespondentID string `json:"respondent_id"`
	Priority     uint8  `json:"priority"`
	EnqueuedMS   int64  `json:"enqueued_at_ms"`
	AttemptN     uint8  `json:"attempt_n"`
	Phone        string `json:"phone"`
	Region       string `json:"region"`
}

// decodeItem parses an encodeItem-emitted JSON blob back into a
// QueueItem. Returns a wrapped error naming the failed field on
// malformed input. UUID fields surface their own parse errors — the
// caller can still wrap with errors.Is on the returned chain.
func decodeItem(data []byte) (api.QueueItem, error) {
	var raw queueItemPayload
	if err := json.Unmarshal(data, &raw); err != nil {
		return api.QueueItem{}, fmt.Errorf("queue/decode: unmarshal: %w", err)
	}
	tenantID, err := uuid.Parse(raw.TenantID)
	if err != nil {
		return api.QueueItem{}, fmt.Errorf("queue/decode: tenant_id=%q: %w", raw.TenantID, err)
	}
	projectID, err := uuid.Parse(raw.ProjectID)
	if err != nil {
		return api.QueueItem{}, fmt.Errorf("queue/decode: project_id=%q: %w", raw.ProjectID, err)
	}
	respondentID, err := uuid.Parse(raw.RespondentID)
	if err != nil {
		return api.QueueItem{}, fmt.Errorf("queue/decode: respondent_id=%q: %w", raw.RespondentID, err)
	}
	return api.QueueItem{
		TenantID:     tenantID,
		ProjectID:    projectID,
		RespondentID: respondentID,
		Priority:     raw.Priority,
		EnqueuedAt:   time.UnixMilli(raw.EnqueuedMS).UTC(),
		AttemptN:     raw.AttemptN,
		Phone:        raw.Phone,
		Region:       raw.Region,
	}, nil
}
