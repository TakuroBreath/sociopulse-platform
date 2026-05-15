//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/internal/surveys/dsl"
	"github.com/sociopulse/platform/internal/surveys/schemavalidator"
	"github.com/sociopulse/platform/pkg/encryption"
	"github.com/sociopulse/platform/tests/smoke"
)

// TestHarness_DialOperator_DialFailureSurfacesError exercises
// smoke.DialOperator against an unreachable endpoint to assert the
// helper surfaces a dial / handshake failure as a non-nil error rather
// than panicking on a nil conn. Plan 21b Task 1 ships the helper; the
// substantive scenario 3 lands in Plan 21b Task 3 against a live cmd/api
// boot.
//
// The point of this self-test is the contract: DialOperator MUST return
// a non-nil error when the TCP dial fails. We aim it at a free port
// nobody is listening on so the kernel sends ECONNREFUSED — exercising
// the error-return path without standing up cmd/api. (Despite the
// "bogus-token" string, no token-validation logic runs here; the dial
// never reaches a WS handler.)
func TestHarness_DialOperator_DialFailureSurfacesError(t *testing.T) {
	t.Parallel()

	addr := smoke.PickFreeAddr(t)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	conn, err := smoke.DialOperator(ctx, t, addr, "bogus-token")
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, err, "DialOperator must surface dial / handshake failure as error")
}

// TestHarness_MinimalValidSurveySchema asserts the fixture bytes returned
// by smoke.MinimalValidSurveySchema parse as JSON and pass the canonical
// SchemaValidator.Validate from internal/surveys/schemavalidator —
// proof that scenario-4 SaveVersion will accept the same body.
func TestHarness_MinimalValidSurveySchema(t *testing.T) {
	t.Parallel()

	schemaBytes := smoke.MinimalValidSurveySchema()
	require.NotEmpty(t, schemaBytes, "MinimalValidSurveySchema must return non-empty bytes")
	require.Greater(t, len(schemaBytes), 2, "schema must be more than '{}'")

	v, err := schemavalidator.New(dsl.NewStubEvaluator())
	require.NoError(t, err, "schemavalidator.New: %v", err)

	report := v.Validate(t.Context(), schemaBytes)
	assert.True(t, report.Valid, "schema must be valid; issues=%v", report.Issues)
}

// TestHarness_BuildRecordingFixture_RoundTrip asserts that
// smoke.BuildRecordingFixture produces a (Ciphertext, Plaintext,
// WrappedDEKHex, KMSKeyID) tuple that round-trips through the
// recording-module crypto stack: we unwrap the DEK, then decrypt the
// audio ciphertext with the canonical recording AAD scopes, and assert
// the plaintext matches the fixture's Plaintext field.
//
// This pins the AAD shape — both DEK ("recording.dek") and audio
// ("recording.audio") — so a future refactor of either scope or the
// BuildAAD encoding fails this test before it breaks scenario 5.
func TestHarness_BuildRecordingFixture_RoundTrip(t *testing.T) {
	t.Parallel()

	stack := smoke.GetSharedStack(t)

	acc := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-RECFIX", "rec-fix-admin", "RecFixPass123!")

	callID := uuid.New()
	fix := smoke.BuildRecordingFixture(t, stack, acc.TenantID, callID)

	require.NotEmpty(t, fix.Ciphertext, "ciphertext must be present")
	require.NotEmpty(t, fix.WrappedDEKHex, "wrapped DEK hex must be present")
	require.NotEmpty(t, fix.SHA256Hex, "sha256 hex must be present")
	require.NotEmpty(t, fix.Plaintext, "plaintext fixture must be present")
	require.Equal(t, "smoke-kek-default", fix.KMSKeyID, "fixture must reference deterministic smoke KEK id")

	// Reconstruct the deterministic 32-byte smoke KEK ("abcd" × 16 hex).
	kek := bytes.Repeat([]byte{0xab, 0xcd}, 16)
	keks := map[string][]byte{fix.KMSKeyID: kek}
	unwrapper := crypto.NewLocalDEKUnwrapper(keks)

	// AAD shape mirrors internal/recording/service/service.go aadScope*.
	dekAAD := encryption.BuildAAD(acc.TenantID, "recording.dek", callID.String())
	audioAAD := encryption.BuildAAD(acc.TenantID, "recording.audio", callID.String())

	wrappedDEK, err := hex.DecodeString(fix.WrappedDEKHex)
	require.NoError(t, err, "decode wrapped DEK hex")

	dekPlain, err := unwrapper.DecryptDEK(t.Context(), fix.KMSKeyID, wrappedDEK, dekAAD)
	require.NoError(t, err, "DecryptDEK")

	plain, err := encryption.Decrypt(dekPlain, fix.Ciphertext, audioAAD)
	require.NoError(t, err, "Decrypt audio")
	assert.Equal(t, fix.Plaintext, plain, "round-tripped plaintext must equal fixture plaintext")
}

// TestHarness_BuildCSVImport_FormatMatches asserts the bytes returned by
// smoke.BuildCSVImport parse as a valid CSV with a "phone" header column
// and at least one data row. Scenario 2 (admin import) feeds these
// bytes through POST /api/projects/:id/respondents/import; the
// production parser (internal/crm/service/import.parseCSV) is
// unexported but mirrors RFC 4180 — here we confirm the header + body
// shape matches the documented format
// (internal/crm/service/import_csv.go::canonicalHeaderAliases).
func TestHarness_BuildCSVImport_FormatMatches(t *testing.T) {
	t.Parallel()

	rows := [][]string{
		{"+79991234567", "Иван Иванов", "ext-001"},
		{"+79991234568", "Петр Петров", "ext-002"},
	}
	csvBytes := smoke.BuildCSVImport(rows)
	require.NotEmpty(t, csvBytes, "BuildCSVImport must return non-empty bytes")

	r := csv.NewReader(bytes.NewReader(csvBytes))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	require.NoError(t, err, "BuildCSVImport bytes must parse as valid CSV")
	require.GreaterOrEqual(t, len(records), 2, "CSV must have header + at least one data row")

	header := records[0]
	hasPhone := false
	for _, h := range header {
		if strings.EqualFold(strings.TrimSpace(h), "phone") {
			hasPhone = true
			break
		}
	}
	assert.True(t, hasPhone, "CSV header must include 'phone' column; got %v", header)
	assert.Len(t, records, len(rows)+1,
		"CSV row count = %d data rows + 1 header", len(rows))
}

// TestHarness_FutureClock_Returns_AddedDuration asserts the closure
// returned by smoke.FutureClock advances time forward by the supplied
// duration. Scenario 8 uses this with 31 days to fast-forward past the
// 30-day soft-delete grace window.
//
// We snapshot time.Now() BOTH before AND after the clock() call and
// assert two-sided bounds:
//
//	nowBefore + offset  <=  got  <=  nowAfter + offset + 1ms
//
// The 1ms upper tolerance covers the gap between the time.Now() call
// inside clock() and the third one we take here for nowAfter. Without
// the two-sided check, a previous revision compared (now+offset) against
// (now+offset-1s) — a tautology that would pass even if FutureClock
// ignored its argument entirely.
func TestHarness_FutureClock_Returns_AddedDuration(t *testing.T) {
	t.Parallel()

	const offset = 31 * 24 * time.Hour
	clock := smoke.FutureClock(offset)
	require.NotNil(t, clock, "FutureClock must return a non-nil closure")

	nowBefore := time.Now()
	got := clock()
	nowAfter := time.Now()

	require.GreaterOrEqual(t, got, nowBefore.Add(offset),
		"FutureClock returned %v < nowBefore+offset %v — offset not applied", got, nowBefore.Add(offset))
	require.LessOrEqual(t, got, nowAfter.Add(offset).Add(time.Millisecond),
		"FutureClock returned %v > nowAfter+offset+1ms %v — offset over-applied", got, nowAfter.Add(offset))
}

// TestHarness_Stack_Reset_TruncatesSeededRow seeds one respondent under
// a fresh tenant, calls Stack.Reset(t), and asserts the row count
// across respondents drops to zero. Reset is the per-test isolation
// seam Phase-1b scenarios depend on; this self-test pins the truncation
// behaviour at the harness boundary so a future regression in the
// TRUNCATE list surfaces here, not at the first scenario that fails to
// observe a clean slate.
//
// TestHarness_Stack_Reset_TruncatesSeededRow does NOT run t.Parallel().
// Stack.Reset TRUNCATEs container-global tables (respondents, calls,
// surveys, call_recordings, etc.) shared across the whole TestMain
// process. A parallel sibling test that inserts into any of those
// tables would lose its rows mid-execution.
//
// All sibling tests in this binary either do NOT touch tables in the
// reset list (PgPool/FutureClock/MinimalValidSurveySchema/DialOperator)
// or consume their rows synchronously in-memory before yielding control
// (BuildRecordingFixture). When ADDING new harness self-tests to this
// binary, mark them sequential (no t.Parallel) if they insert into any
// of the tables enumerated in resetTables in stack.go.
func TestHarness_Stack_Reset_TruncatesSeededRow(t *testing.T) { //nolint:paralleltest // TRUNCATE is container-global; parallel siblings would lose rows mid-execution
	stack := smoke.GetSharedStack(t)

	acc := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-RESET", "reset-admin", "ResetPass123!")
	projectID := smoke.SeedProject(t, stack, acc.TenantID, "reset-proj", "Reset Project")

	conn, err := pgx.Connect(t.Context(), stack.PostgresDSN)
	require.NoError(t, err, "open pg conn")
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	respondentID := uuid.New()
	_, err = conn.Exec(t.Context(),
		`INSERT INTO respondents (
			id, tenant_id, project_id, phone_encrypted, phone_hash,
			region_code, source
		 ) VALUES ($1, $2, $3, $4, $5, '', 'imported')`,
		respondentID, acc.TenantID, projectID,
		[]byte("encrypted-phone-stub"), []byte("smoke-hash-stub-32-bytes-padding!!"))
	require.NoError(t, err, "insert respondent")

	var preCount int
	require.NoError(t,
		conn.QueryRow(t.Context(), `SELECT count(*) FROM respondents WHERE id = $1`, respondentID).
			Scan(&preCount))
	require.Equal(t, 1, preCount, "respondent row must exist before Reset")

	stack.Reset(t)

	var postCount int
	require.NoError(t,
		conn.QueryRow(t.Context(), `SELECT count(*) FROM respondents`).Scan(&postCount))
	assert.Equal(t, 0, postCount, "respondents table must be empty after Reset")
}
