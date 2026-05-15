//go:build smoke

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/internal/recording/storage"
	recwire "github.com/sociopulse/platform/internal/recording/wire"
	"github.com/sociopulse/platform/tests/smoke"
)

// TestSmoke_RecordingSearchAndStream — Plan 21b Task 5.
//
// End-to-end smoke proving the recording module's read pipeline against
// a fully-booted cmd/api + Postgres testcontainer:
//
//	HTTP /api/recordings/search ─►  RecordingService.Search
//	                            ─►  store PostgresStore (RLS-scoped SELECT)
//	                            ─►  SearchResponse{items[], next_cursor, has_more}
//
//	HTTP /api/calls/:id/recording ─►  RecordingService.OpenAudioStream
//	                              ─►  store.GetByCallID (RLS)
//	                              ─►  crypto.LocalDEKUnwrapper.DecryptDEK
//	                                  (KEK = smoke-kek-default, AAD = "recording.dek")
//	                              ─►  storage.LocalObjectStore.Get
//	                              ─►  crypto.AESGCMDecryptor.Decrypt
//	                                  (AAD = "recording.audio")
//	                              ─►  audio/ogg body with full plaintext
//
// Catches:
//
//   - sha256 chain-of-custody (ADR-0005): the value persisted in
//     call_recordings.sha256_hex is the sha256 of the CIPHERTEXT (what
//     the client stored), and the search response surfaces it unchanged.
//   - KMS/storage wiring: the smoke override seam injects ONE
//     *recwire.Ports into both the test seeder AND cmd/api boot so the
//     ciphertext put by the test is decryptable by the same DEK the
//     handler unwraps. A drift in `aadScopeRecording*` or in the KEK id
//     mapping fails the audio decrypt and surfaces as 500 here.
//   - Range-header rejection (ADR-0005 §15.4): GET with
//     `Range: bytes=0-1023` MUST 416 — AES-256-GCM authenticates the
//     full ciphertext, partial-content delivery would either bypass
//     the auth-tag or require re-decrypting the whole object only to
//     slice. The production handler was hardened in this same commit
//     to reject Range explicitly (previously it only advertised
//     `Accept-Ranges: none` but did not actively reject the header).
//   - Cross-tenant isolation: tenant-B JWT against tenant-A's recording
//     row → 404 (RLS + ErrNotFound), and the search response for the
//     B-scoped JWT returns an empty page.
//
// Verified contracts (read from source BEFORE writing):
//
//   - Routes (internal/recording/transport/http/routes.go:64-69):
//     GET /api/calls/:id/recording          (admin OR supervisor)
//     GET /api/recordings/search            (admin OR supervisor)
//   - Stream response shape (recording_handler.go:48-66):
//     200 + audio/ogg body + Content-Length + Accept-Ranges:none +
//     Cache-Control:private, no-store; on Range header → 416 +
//     ErrorEnvelope{code:"recording.range_not_satisfiable"} +
//     Content-Range:"bytes */*" + Accept-Ranges:none. The full
//     response body is the decrypted plaintext (matches
//     fixture.Plaintext bytewise).
//   - Search response shape (dto.go:15-19, 25-41): SearchResponse{
//     items: [{recording_id, call_id, tenant_id, bytes_size,
//     duration_ms, sha256 (hex), status, committed_at, ...}],
//     next_cursor, has_more}. sha256 field is named "sha256" on the
//     wire (NOT "sha256_hex") even though the DB column is
//     `sha256_hex`.
//   - Recording wire ports (recwire.Ports{DEK, Objects}): the smoke
//     LocalDEKUnwrapper is registered under id "smoke-kek-default"
//     with the deterministic 32-byte KEK matching
//     tests/smoke/recording_seed.go::smokeKEKHex; the LocalObjectStore
//     is empty at construction and gets the fixture ciphertext via
//     SeedRecording.
//   - smoke seam (cmd/api/smoke_overrides.go):
//     SetSmokeRecordingPorts(p) must be called BEFORE bootAPI so
//     buildRecordingPorts (recording.go:58) returns the shared *Ports;
//     production builds (no smoke tag) compile the !smoke twin and
//     fall through to recwire.LocalPorts unchanged.
func TestSmoke_RecordingSearchAndStream(t *testing.T) {
	t.Parallel()

	stack := smoke.GetSharedStack(t)

	// Step 1: build the shared *recwire.Ports BEFORE bootAPI so cmd/api's
	// buildRecordingPorts picks it up via the atomic.Pointer seam.
	//
	// The KEK matches tests/smoke/recording_seed.go::smokeKEKHex
	// ("abcd"×16 hex = 32 bytes after decode) so the LocalDEKUnwrapper
	// recognises the deterministic id every smoke fixture references.
	// A drift in either of these two locations breaks the round-trip
	// silently — the harness self-test in tests/smoke/harness_test.go
	// pins the contract from the seed side; this scenario pins it from
	// the cmd/api side.
	kek := bytes.Repeat([]byte{0xab, 0xcd}, 16)
	require.Len(t, kek, 32, "smoke KEK must be 32 bytes (AES-256)")
	ports := &recwire.Ports{
		DEK:     crypto.NewLocalDEKUnwrapper(map[string][]byte{"smoke-kek-default": kek}),
		Objects: storage.NewLocalObjectStore(),
	}
	SetSmokeRecordingPorts(ports)
	// t.Cleanup resets the seam so a subsequent parallel scenario that
	// also boots cmd/api (e.g. a future Task-7+ scenario) does NOT
	// inherit this *Ports instance. The atomic.Pointer's Load returning
	// nil restores the LocalPorts fall-through path.
	t.Cleanup(func() { SetSmokeRecordingPorts(nil) })

	httpAddr, _ := bootAPI(t, stack)

	// Two tenants for the cross-tenant regression net. Each one is
	// SeedTenantAndAdmin'd so the seeder mints a tenants + users row
	// with kms_kek_id = "smoke-kek-default" — the LocalDEKUnwrapper
	// recognises that id for both tenants (single KEK shared across
	// the smoke harness; production minted per-tenant KEKs via the
	// tenancy module's create flow).
	tenantA := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-REC-A", "rec-admin-a", "RecPassA123!")
	tenantB := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-REC-B", "rec-admin-b", "RecPassB123!")

	projectA := smoke.SeedProject(t, stack, tenantA.TenantID, "smoke-rec-proj", "Recording smoke project")
	callA := smoke.SeedCall(t, stack, tenantA.TenantID, projectA)

	// Step 3 + 4: build the fixture, seed the row + put the ciphertext
	// through the SAME *LocalObjectStore the cmd/api process is using.
	// GetSmokeRecordingPorts() recovers the *Ports we installed above;
	// passing ports.Objects directly works too but using the accessor
	// matches the documented pattern in tests/smoke/recording_seed.go.
	got := GetSmokeRecordingPorts()
	require.NotNil(t, got, "GetSmokeRecordingPorts must return the *Ports installed by SetSmokeRecordingPorts (atomic.Pointer Load)")
	require.Same(t, ports, got, "atomic.Pointer round-trip must return the SAME instance — a Load returning a copy would silently break the shared-store contract")

	fixture := smoke.BuildRecordingFixture(t, stack, tenantA.TenantID, callA)
	require.NotEmpty(t, fixture.Ciphertext, "fixture ciphertext must be non-empty (encrypt succeeded)")
	require.NotEmpty(t, fixture.Plaintext, "fixture plaintext must be non-empty")
	require.Len(t, fixture.SHA256Hex, 64, "sha256 hex must be 64 chars")

	_ = smoke.SeedRecording(t, stack, ports.Objects, tenantA.TenantID, callA, fixture)

	ctx := t.Context()
	cli := &http.Client{Timeout: 15 * time.Second}
	baseURL := "http://" + httpAddr

	adminAJWT := loginAndAccessToken(ctx, t, cli, httpAddr, tenantA)
	adminBJWT := loginAndAccessToken(ctx, t, cli, httpAddr, tenantB)

	// Step 6: GET /api/recordings/search with admin-A JWT → 200 +
	// SearchResponse with exactly one item; assert sha256 matches the
	// fixture's ciphertext-sha (ADR-0005 chain-of-custody).
	searchStatus, searchBytes := getWithJWT(ctx, t, cli,
		baseURL+"/api/recordings/search", adminAJWT)
	require.Equalf(t, http.StatusOK, searchStatus,
		"GET /api/recordings/search must 200 for admin; got %d body=%s",
		searchStatus, string(searchBytes))

	// Decode into a local schema that mirrors transport/http/dto.go's
	// SearchResponse / RecordingMetadataDTO. We avoid importing the
	// transport package (depguard `module-boundaries` forbids
	// internal/recording/transport from non-cmd packages, and a
	// build-tagged smoke test counts as a package consumer here) by
	// duplicating the relevant subset; the field tags are the wire
	// contract and a future shape drift surfaces as a decode error.
	type recItem struct {
		RecordingID uuid.UUID `json:"recording_id"`
		CallID      uuid.UUID `json:"call_id"`
		TenantID    uuid.UUID `json:"tenant_id"`
		BytesSize   int64     `json:"bytes_size"`
		DurationMS  int64     `json:"duration_ms"`
		SHA256Hex   string    `json:"sha256"`
		Status      string    `json:"status"`
	}
	type searchResp struct {
		Items      []recItem `json:"items"`
		NextCursor string    `json:"next_cursor"`
		HasMore    bool      `json:"has_more"`
	}
	var sa searchResp
	require.NoErrorf(t, json.Unmarshal(searchBytes, &sa),
		"decode SearchResponse: %s", string(searchBytes))
	require.Lenf(t, sa.Items, 1,
		"tenant-A search must return exactly one item (the seeded recording); got %d items body=%s",
		len(sa.Items), string(searchBytes))
	item := sa.Items[0]
	assert.Equal(t, callA, item.CallID, "search item must echo the seeded call_id")
	assert.Equal(t, tenantA.TenantID, item.TenantID,
		"search item must carry the tenant_id the row was inserted under")
	assert.Equal(t, fixture.SHA256Hex, item.SHA256Hex,
		"search item sha256 must equal sha256(ciphertext) per ADR-0005")
	assert.Equal(t, "stored", item.Status,
		"freshly-seeded recording must have status=stored")
	assert.Equal(t, int64(len(fixture.Ciphertext)), item.BytesSize,
		"bytes_size must equal the ciphertext byte count")
	assert.False(t, sa.HasMore, "one-item page must report has_more=false")

	// Step 6 (cross-tenant): admin-B's search MUST NOT see tenant-A's
	// row. RLS at the store + transport-level claims.TenantID drive the
	// scoping; an empty Items array proves both paths short-circuited.
	searchBStatus, searchBBytes := getWithJWT(ctx, t, cli,
		baseURL+"/api/recordings/search", adminBJWT)
	require.Equalf(t, http.StatusOK, searchBStatus,
		"tenant-B search must 200 (empty page, not 4xx); got %d body=%s",
		searchBStatus, string(searchBBytes))
	var sb searchResp
	require.NoErrorf(t, json.Unmarshal(searchBBytes, &sb),
		"decode tenant-B SearchResponse: %s", string(searchBBytes))
	assert.Emptyf(t, sb.Items,
		"tenant-B must not observe tenant-A's recording; got %d items body=%s",
		len(sb.Items), string(searchBBytes))

	// Step 7: GET /api/calls/:callA/recording with admin-A JWT → 200 +
	// the FULL plaintext audio. Headers from recording_handler.go:48-51:
	//   Content-Type: audio/ogg (codec=opus → audio/ogg per
	//                            contentTypeForCodec in service.go)
	//   Content-Length: <plaintext length>
	//   Accept-Ranges: none
	//   Cache-Control: private, no-store
	streamURL := baseURL + "/api/calls/" + callA.String() + "/recording"
	stream := streamGetWithJWT(ctx, t, cli, streamURL, adminAJWT)
	require.Equalf(t, http.StatusOK, stream.status,
		"GET stream must 200 for admin; got %d body=%s",
		stream.status, string(stream.body))
	assert.Equal(t, "audio/ogg", stream.header.Get("Content-Type"),
		"opus codec → audio/ogg content-type (contentTypeForCodec)")
	assert.Equal(t, "none", stream.header.Get("Accept-Ranges"),
		"Accept-Ranges must be 'none' (ADR-0005 §15.4)")
	assert.Equal(t, "private, no-store", stream.header.Get("Cache-Control"),
		"chain-of-custody material must not be cached")
	assert.Equal(t, fmt.Sprintf("%d", len(fixture.Plaintext)),
		stream.header.Get("Content-Length"),
		"Content-Length must equal plaintext length (post-decrypt)")

	// The body MUST be the decrypted plaintext byte-for-byte. Compare
	// sha256 sums first (cheap on failure — only emit 64-char hex pair
	// to the diagnostic) and then assert bytewise equality.
	wantSHA := sha256.Sum256(fixture.Plaintext)
	gotSHA := sha256.Sum256(stream.body)
	assert.Equalf(t, hex.EncodeToString(wantSHA[:]), hex.EncodeToString(gotSHA[:]),
		"streamed plaintext sha256 must equal fixture plaintext sha256 — a mismatch means the AES-GCM round-trip is wrong (wrong key, wrong AAD, wrong ciphertext bytes)")
	assert.Equal(t, fixture.Plaintext, stream.body,
		"streamed body must be bytewise equal to fixture plaintext")

	// Step 8: Range header → 416 (ADR-0005 §15.4 contract).
	// AES-256-GCM authenticates the trailing tag over the whole
	// ciphertext, so partial-content delivery would either bypass the
	// auth-tag check or require fully decrypting the object before
	// slicing — neither matches the chain-of-custody guarantee. The
	// production handler was hardened in this same commit to reject
	// the header explicitly.
	ranged := streamGetWithRange(ctx, t, cli, streamURL, adminAJWT, "bytes=0-1023")
	require.Equalf(t, http.StatusRequestedRangeNotSatisfiable, ranged.status,
		"GET stream with Range header must 416 (ADR-0005 §15.4); got %d body=%s",
		ranged.status, string(ranged.body))
	assert.Equal(t, "none", ranged.header.Get("Accept-Ranges"),
		"Accept-Ranges must remain 'none' on the 416 response")
	assert.Equal(t, "bytes */*", ranged.header.Get("Content-Range"),
		"Content-Range hint per RFC 7233 §4.4 ('*' size since the handler short-circuits before looking up the row)")
	// Body should be the canonical ErrorEnvelope; don't over-assert on
	// the message text — pin only the stable code.
	var errEnv struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	require.NoErrorf(t, json.Unmarshal(ranged.body, &errEnv),
		"416 response body must decode as ErrorEnvelope: %s", string(ranged.body))
	assert.Equal(t, "recording.range_not_satisfiable", errEnv.Code,
		"416 envelope code must be the canonical recording.range_not_satisfiable")

	// Step 9: cross-tenant stream attempt → 404.
	// renderServiceError maps rapi.ErrNotFound → 404 +
	// {"code":"recording.not_found"} (errors.go:39-43). The store's
	// GetByCallID returns ErrCallNotFound for a row outside the
	// tenant scope (RLS at the SELECT level + the transport claim
	// driving the lookup) — ErrCallNotFound bubbles up as
	// ErrNotFound through the service.
	xtStatus, xtBody := getWithJWT(ctx, t, cli, streamURL, adminBJWT)
	require.Equalf(t, http.StatusNotFound, xtStatus,
		"cross-tenant stream attempt must 404 (RLS + claim mismatch); got %d body=%s",
		xtStatus, string(xtBody))
	require.NoErrorf(t, json.Unmarshal(xtBody, &errEnv),
		"404 response body must decode as ErrorEnvelope: %s", string(xtBody))
	assert.Equal(t, "recording.not_found", errEnv.Code,
		"cross-tenant 404 envelope code must be recording.not_found")
}

// streamResult bundles the fields of a stream-recording HTTP response
// the scenario asserts on. We expose only what's needed (status, header
// snapshot, body bytes) so the helper closes the *http.Response.Body
// before returning — the bodyclose linter flags helpers that hand the
// *Response back to the caller.
//
// header is taken via http.Header.Clone so a future helper reusing the
// same response value cannot accidentally mutate the captured snapshot.
type streamResult struct {
	status int
	header http.Header
	body   []byte
}

// streamGetWithJWT issues a GET against url with the supplied JWT and
// returns the status / header snapshot / fully-consumed body bytes.
// Mirrors getWithJWT in smoke_test.go but additionally surfaces the
// response headers — the recording-stream assertions check
// Content-Type / Content-Length / Accept-Ranges / Cache-Control
// alongside the body bytes. The body is read into memory and the
// underlying response is closed before return, so the caller never
// holds a live connection (bodyclose-friendly).
//
// Transport failures (build / dial / read) fail the test via require —
// they are never the assertion under test, and a nil response would
// only cause a confusing nil-deref one line later.
func streamGetWithJWT(ctx context.Context, t *testing.T, cli *http.Client, url, jwt string) streamResult {
	t.Helper()
	return doStreamGet(ctx, t, cli, url, jwt, "")
}

// streamGetWithRange is streamGetWithJWT plus an explicit Range header.
// Used by the 416-rejection assertion — the production handler must
// short-circuit BEFORE invoking RecordingService.OpenAudioStream when
// any Range header is present (ADR-0005 §15.4).
func streamGetWithRange(ctx context.Context, t *testing.T, cli *http.Client, url, jwt, rangeValue string) streamResult {
	t.Helper()
	return doStreamGet(ctx, t, cli, url, jwt, rangeValue)
}

// doStreamGet is the shared implementation of the two stream GET
// helpers. Splitting the optional-Range flag into one private function
// keeps the public helpers ergonomic at the call site (the scenario
// reads "streamGetWithJWT" / "streamGetWithRange" by name) without
// duplicating the close-before-return body-read logic.
func doStreamGet(ctx context.Context, t *testing.T, cli *http.Client, url, jwt, rangeValue string) streamResult {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err, "build GET %s", url)
	req.Header.Set("Authorization", "Bearer "+jwt)
	if rangeValue != "" {
		req.Header.Set("Range", rangeValue)
	}
	resp, err := cli.Do(req)
	require.NoError(t, err, "GET %s", url)
	defer func() { _ = resp.Body.Close() }()
	buf, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read body for GET %s", url)
	return streamResult{
		status: resp.StatusCode,
		header: resp.Header.Clone(),
		body:   buf,
	}
}
