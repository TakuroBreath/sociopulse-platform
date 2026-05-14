//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	// Native ClickHouse driver registration. We don't use database/sql
	// here, but the blank import keeps the integration suite self-
	// contained (matches the cmd/migrator suite).
	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/clickhouse"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"go.uber.org/zap"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/metrics"
	"github.com/sociopulse/platform/internal/analytics/service"
	"github.com/sociopulse/platform/internal/analytics/store"
	recordingapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// chImage pins ClickHouse to the same tag used by the store
// integration suite (Plan 13.1). Bumping floats reproducibility.
const chImage = "clickhouse/clickhouse-server:24.8"

// chDSNs bundles the migrate-flavoured DSN (carries
// x-multi-statement=true) and the verify DSN (bare) — see Plan 13.1
// production lesson #4.
type chDSNs struct {
	migrate string
	verify  string
}

// startCH boots a fresh CH container and returns the DSN pair. Cleanup
// is registered via t.Cleanup.
func startCH(t *testing.T) chDSNs {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	ch, err := tcclickhouse.Run(ctx, chImage,
		tcclickhouse.WithDatabase("sociopulse_test"),
		tcclickhouse.WithUsername("test"),
		tcclickhouse.WithPassword("test"),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = ch.Terminate(context.Background())
	})

	migrateDSN, err := ch.ConnectionString(ctx, "x-multi-statement=true")
	require.NoError(t, err)
	verifyDSN, err := ch.ConnectionString(ctx)
	require.NoError(t, err)
	return chDSNs{migrate: migrateDSN, verify: verifyDSN}
}

// migrateUp applies every CH migration in migrations/clickhouse against
// the migrate-flavoured DSN.
func migrateUp(t *testing.T, dsn string) {
	t.Helper()
	absMigrations, err := filepath.Abs(filepath.Join("..", "..", "..", "migrations", "clickhouse"))
	require.NoError(t, err)

	m, err := migrate.New("file://"+absMigrations, dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = m.Close()
	})
	require.NoError(t, m.Up())
}

// startEmbeddedNATS boots an in-process NATS server with JetStream on
// a random port. Pattern duplicated from pkg/eventbus/helpers_test.go
// (the original is unexported).
func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	storeDir := filepath.Join(t.TempDir(), "jetstream")
	opts := &server.Options{
		Host:                  "127.0.0.1",
		Port:                  -1,
		NoLog:                 true,
		NoSigs:                true,
		MaxControlLine:        4096,
		DisableShortFirstPing: true,
		JetStream:             true,
		StoreDir:              storeDir,
	}
	srv, err := server.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatal("embedded NATS server did not become ready in 5s")
	}
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	return srv.ClientURL()
}

// ensureStream provisions a JetStream stream with InterestPolicy
// retention so messages are dropped after ack — keeps the embedded
// store tiny.
func ensureStream(t *testing.T, url, name string, subjects []string) {
	t.Helper()
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	defer nc.Close()
	js, err := nc.JetStream()
	require.NoError(t, err)
	cfg := &nats.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: nats.InterestPolicy,
		Storage:   nats.MemoryStorage,
		MaxAge:    1 * time.Minute,
	}
	_, err = js.AddStream(cfg)
	if err != nil && errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
		_, err = js.UpdateStream(cfg)
	}
	require.NoError(t, err, "ensure stream %q", name)
}

// awaitCount polls a counter callback until it returns the target
// value or the deadline expires. Used to assert eventual consistency
// between the bus publish, the pipeline flush, and the CH SELECT.
func awaitCount(t *testing.T, deadline time.Duration, target uint64, get func() uint64) {
	t.Helper()
	to := time.NewTimer(deadline)
	defer to.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if got := get(); got == target {
			return
		}
		select {
		case <-tick.C:
			continue
		case <-to.C:
			t.Fatalf("awaitCount: deadline exceeded after %s, last got=%d, want=%d", deadline, get(), target)
		}
	}
}

// chRowCount queries CH for the rowcount of the named table filtered
// by tenant_id. Returns 0 on driver error so awaitCount keeps polling.
func chRowCount(t *testing.T, conn *store.Conn, table string, tenantID uuid.UUID) uint64 {
	t.Helper()
	var c uint64
	row := conn.Driver().QueryRow(t.Context(),
		"SELECT count() FROM "+table+" WHERE tenant_id = ?",
		tenantID,
	)
	if err := row.Scan(&c); err != nil {
		t.Logf("chRowCount %q: scan error: %v (will retry)", table, err)
		return 0
	}
	return c
}

// TestIngest_EndToEnd_AllSubjects runs the happy path through real
// NATS JetStream + real ClickHouse:
//  1. Embedded NATS booted on a random port.
//  2. Streams ANALYTICS (cross-tenant subjects) + RECORDING
//     (per-tenant wildcard) provisioned.
//  3. CH container booted + migrated.
//  4. IngestPipeline wired with a real bus + StoreAdapter.
//  5. N events of each kind published; assert CH eventually shows the
//     expected counts.
//  6. Cancel ctx + assert clean shutdown (Run returns context.Canceled).
func TestIngest_EndToEnd_AllSubjects(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	natsURL := startEmbeddedNATS(t)
	pub, err := eventbus.NewNATSPublisher(ctx, []string{natsURL}, "")
	require.NoError(t, err)
	sub, err := eventbus.NewNATSSubscriber(ctx, []string{natsURL}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })
	t.Cleanup(func() { _ = sub.Close() })

	// Cross-tenant analytics subjects in one stream; per-tenant
	// recording.uploaded under tenant.> in another. Two streams keeps
	// the subject-binding test honest — the wildcard subscriber sees
	// only the recording subject's messages.
	ensureStream(t, natsURL, "ANALYTICS", []string{
		apianalytics.SubjectCallsAnalytics,
		apianalytics.SubjectOperatorStateAnalytics,
	})
	ensureStream(t, natsURL, "RECORDING", []string{"tenant.>"})

	dsns := startCH(t)
	migrateUp(t, dsns.migrate)
	conn, err := store.Open(ctx, store.Config{
		DSN:           dsns.verify,
		BatchSize:     100,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	p, err := service.NewIngestPipeline(
		sub,
		&service.StoreAdapter{Conn: conn},
		zap.NewNop(),
		nil,
		service.IngestConfig{
			BatchSize:     5,
			FlushInterval: 200 * time.Millisecond,
			DedupSize:     1000,
		},
	)
	require.NoError(t, err)

	runCtx, runCancel := context.WithCancel(ctx)
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- p.Run(runCtx) }()

	// Give the pipeline a moment to register the 3 subscribers before
	// publishing — otherwise the first publish lands while DeliverNew
	// has no consumer to dispatch to (it would still be delivered
	// later, but the assertion deadline would fire first).
	time.Sleep(200 * time.Millisecond)

	tenantID := uuid.New()
	projectID := uuid.New()

	// 10 call events. Distinct event_ids so dedup doesn't drop any.
	for range 10 {
		ev := apianalytics.AnalyticsCallEventPayload{
			Date:        "2026-05-14",
			TS:          time.Now().UTC(),
			TenantID:    tenantID,
			ProjectID:   projectID,
			OperatorID:  uuid.New(),
			CallID:      uuid.New(),
			Status:      "success",
			DurationSec: 60,
			HangupCause: "NORMAL_CLEARING",
			RegionCode:  "MSK",
			AttemptNo:   1,
			TrunkUsed:   "trunk-a",
			EventID:     uuid.New(),
		}
		raw, err := json.Marshal(ev)
		require.NoError(t, err)
		require.NoError(t, pub.Publish(ctx, apianalytics.SubjectCallsAnalytics, raw))
	}

	// 5 operator_state events.
	for range 5 {
		ev := apianalytics.AnalyticsOperatorStateEventPayload{
			Date:               "2026-05-14",
			TS:                 time.Now().UTC(),
			TenantID:           tenantID,
			UserID:             uuid.New(),
			State:              "ready",
			DurationInStateSec: 30,
			EventID:            uuid.New(),
		}
		raw, err := json.Marshal(ev)
		require.NoError(t, err)
		require.NoError(t, pub.Publish(ctx, apianalytics.SubjectOperatorStateAnalytics, raw))
	}

	// 3 recording.uploaded events on the per-tenant subject.
	recSubject := recordingapi.SubjectRecordingUploadedFor(tenantID)
	for range 3 {
		ev := recordingapi.RecordingUploadedEvent{
			RecordingID:        uuid.New(),
			CallID:             uuid.New(),
			TenantID:           tenantID,
			ProjectID:          projectID,
			FSNode:             "fs-01",
			S3Key:              "tenant/abc/recordings/xyz.bin",
			EncryptionKeyAlias: "kms-alias-a",
			EventID:            uuid.New(),
			BytesSize:          12345,
			DurationMS:         60000,
			DurationSec:        60,
			SHA256Hex:          "deadbeef",
			Status:             "stored",
			CommittedAt:        time.Now().Unix(),
		}
		raw, err := json.Marshal(ev)
		require.NoError(t, err)
		require.NoError(t, pub.Publish(ctx, recSubject, raw))
	}

	awaitCount(t, 10*time.Second, 10, func() uint64 { return chRowCount(t, conn, "events_calls", tenantID) })
	awaitCount(t, 10*time.Second, 5, func() uint64 { return chRowCount(t, conn, "events_operator_state", tenantID) })
	awaitCount(t, 10*time.Second, 3, func() uint64 { return chRowCount(t, conn, "events_recording_uploaded", tenantID) })

	runCancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngest_DrainOnContextDone_FlushesResidualBuffers asserts the
// drain path against real CH: publish fewer rows than BatchSize, let
// FlushInterval stay very large, cancel, and verify the rows appear
// in CH (i.e. the drain phase flushed them under its own context).
func TestIngest_DrainOnContextDone_FlushesResidualBuffers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	natsURL := startEmbeddedNATS(t)
	pub, err := eventbus.NewNATSPublisher(ctx, []string{natsURL}, "")
	require.NoError(t, err)
	sub, err := eventbus.NewNATSSubscriber(ctx, []string{natsURL}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })
	t.Cleanup(func() { _ = sub.Close() })

	ensureStream(t, natsURL, "ANALYTICS", []string{
		apianalytics.SubjectCallsAnalytics,
		apianalytics.SubjectOperatorStateAnalytics,
	})
	ensureStream(t, natsURL, "RECORDING", []string{"tenant.>"})

	dsns := startCH(t)
	migrateUp(t, dsns.migrate)
	conn, err := store.Open(ctx, store.Config{
		DSN:           dsns.verify,
		BatchSize:     100,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	p, err := service.NewIngestPipeline(
		sub,
		&service.StoreAdapter{Conn: conn},
		zap.NewNop(),
		nil,
		service.IngestConfig{
			BatchSize:     1000,             // count threshold won't trip
			FlushInterval: 10 * time.Second, // ticker won't trip in test window
			DedupSize:     1000,
			DrainTimeout:  5 * time.Second,
		},
	)
	require.NoError(t, err)

	runCtx, runCancel := context.WithCancel(ctx)
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- p.Run(runCtx) }()

	time.Sleep(200 * time.Millisecond) // wait for Subscribe to register

	tenantID := uuid.New()
	projectID := uuid.New()

	// Publish 5 events — well below BatchSize=1000.
	for range 5 {
		ev := apianalytics.AnalyticsCallEventPayload{
			Date:        "2026-05-14",
			TS:          time.Now().UTC(),
			TenantID:    tenantID,
			ProjectID:   projectID,
			OperatorID:  uuid.New(),
			CallID:      uuid.New(),
			Status:      "success",
			DurationSec: 60,
			HangupCause: "NORMAL_CLEARING",
			RegionCode:  "MSK",
			AttemptNo:   1,
			TrunkUsed:   "trunk-a",
			EventID:     uuid.New(),
		}
		raw, err := json.Marshal(ev)
		require.NoError(t, err)
		require.NoError(t, pub.Publish(ctx, apianalytics.SubjectCallsAnalytics, raw))
	}

	// Give the bus's push goroutine a moment to deliver all 5 to the
	// handler. The events are buffered (BatchSize=1000 won't trip) but
	// the LRU + buffer state must be populated before we cancel.
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, uint64(0), chRowCount(t, conn, "events_calls", tenantID),
		"no flush should have fired yet (count + time thresholds both untouched)")

	runCancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
	// After Run returns, the drain has completed — all 5 rows must be
	// in CH.
	require.Equal(t, uint64(5), chRowCount(t, conn, "events_calls", tenantID),
		"drain must flush all 5 buffered rows")
}

// chRowCountFINAL is the deduped-view analogue of chRowCount. Reads
// `SELECT count() FROM <table> FINAL` to force ReplacingMergeTree's
// async dedup-on-merge to converge before counting. Used by the
// duplicate-replay test below — without FINAL, two rows with the same
// event_id can both be visible until the next background merge.
func chRowCountFINAL(t *testing.T, conn *store.Conn, table string, tenantID uuid.UUID) uint64 {
	t.Helper()
	var c uint64
	row := conn.Driver().QueryRow(t.Context(),
		"SELECT count() FROM "+table+" FINAL WHERE tenant_id = ?",
		tenantID,
	)
	if err := row.Scan(&c); err != nil {
		t.Logf("chRowCountFINAL %q: scan error: %v (will retry)", table, err)
		return 0
	}
	return c
}

// chRowCountWithCtx mirrors chRowCount but takes an explicit ctx so it
// can be used from a closure that already owns a context — chRowCount
// derives from t.Context() which contextcheck flags as a context leak
// when invoked from a nested closure carrying its own ctx.
func chRowCountWithCtx(ctx context.Context, t *testing.T, conn *store.Conn, table string, tenantID uuid.UUID) uint64 {
	t.Helper()
	var c uint64
	row := conn.Driver().QueryRow(ctx,
		"SELECT count() FROM "+table+" WHERE tenant_id = ?",
		tenantID,
	)
	if err := row.Scan(&c); err != nil {
		t.Logf("chRowCountWithCtx %q: scan error: %v (will retry)", table, err)
		return 0
	}
	return c
}

// TestIngest_DuplicateEventsReplaced_AfterMerge asserts that the
// migration to ReplacingMergeTree(_inserted_at) ORDER BY (tenant_id,
// event_id) deduplicates rows that share an event_id when the table
// is forced through OPTIMIZE … FINAL or queried with SELECT … FINAL.
//
// Scenario: bypass the LRU entirely by writing the same event payload
// twice directly via the StoreAdapter (a real CH dup is what happens
// when consumer A and consumer B in two replicas both see the same
// NATS redelivery and both pass their own cold LRUs). Force a merge
// with OPTIMIZE TABLE … FINAL, then SELECT FINAL and assert count == 1.
// Without ReplacingMergeTree the count would be 2.
func TestIngest_DuplicateEventsReplaced_AfterMerge(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dsns := startCH(t)
	migrateUp(t, dsns.migrate)
	conn, err := store.Open(ctx, store.Config{
		DSN:           dsns.verify,
		BatchSize:     100,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	adapter := &service.StoreAdapter{Conn: conn}

	tenantID := uuid.New()
	projectID := uuid.New()
	eventID := uuid.New()
	callID := uuid.New()

	mkRow := func() apianalytics.AnalyticsCallEventPayload {
		return apianalytics.AnalyticsCallEventPayload{
			Date:        "2026-05-14",
			TS:          time.Now().UTC(),
			TenantID:    tenantID,
			ProjectID:   projectID,
			OperatorID:  uuid.New(),
			CallID:      callID,
			Status:      "success",
			DurationSec: 60,
			HangupCause: "NORMAL_CLEARING",
			RegionCode:  "MSK",
			AttemptNo:   1,
			TrunkUsed:   "trunk-a",
			EventID:     eventID, // same event_id for both writes
		}
	}

	// First insert: brand new row.
	require.NoError(t, adapter.InsertCalls(ctx, []apianalytics.AnalyticsCallEventPayload{mkRow()}))
	// Slight wall-clock delay so the DateTime64(3) DEFAULT now64() value
	// differs between the two writes — ReplacingMergeTree keeps the row
	// with the LARGEST _inserted_at per ORDER BY tuple. A 5ms gap is
	// well above DateTime64(3)'s 1ms granularity.
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, adapter.InsertCalls(ctx, []apianalytics.AnalyticsCallEventPayload{mkRow()}))

	// Without FINAL: two rows visible (ReplacingMergeTree dedups on
	// merge, not on insert).
	awaitCount(t, 5*time.Second, 2, func() uint64 {
		return chRowCount(t, conn, "events_calls", tenantID)
	})

	// Force the dedup-merge so SELECT … FINAL converges immediately.
	require.NoError(t, conn.Driver().Exec(ctx, "OPTIMIZE TABLE events_calls FINAL"))

	// After FINAL: exactly one row per (tenant_id, event_id).
	require.Equal(t, uint64(1), chRowCountFINAL(t, conn, "events_calls", tenantID),
		"ReplacingMergeTree must collapse to 1 row per (tenant_id, event_id) under SELECT FINAL")
}

// TestIngest_DedupMissCounterIncrements_OnLRUColdRestart asserts that
// the new sociopulse_analytics_ingest_dedup_miss_total counter ticks
// when the LRU is cold (the pipeline just started or was restarted)
// and a row arrives whose event_id is NOT in the in-memory LRU. This is
// the canonical "we don't know if this is a redelivery from before we
// were running" signal — ReplacingMergeTree will reconcile at merge
// time but the metric tells ops the rate of unverifiable inserts so
// they can tune LRU size.
//
// Scenario:
//  1. Boot the pipeline with a fresh LRU; publish one event; assert it
//     lands in CH; assert dedup_miss_total ticks once (the cold-start
//     miss).
//  2. Cancel the pipeline, boot a SECOND pipeline with its own fresh
//     LRU, publish the SAME event_id; assert dedup_miss_total ticks
//     AGAIN — even though the row is a true duplicate at the CH
//     storage layer, the in-memory LRU has no record of it and the
//     pipeline treats it as a cold-start add. ReplacingMergeTree
//     reconciles via OPTIMIZE FINAL below.
func TestIngest_DedupMissCounterIncrements_OnLRUColdRestart(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	natsURL := startEmbeddedNATS(t)
	pub, err := eventbus.NewNATSPublisher(ctx, []string{natsURL}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	ensureStream(t, natsURL, "ANALYTICS", []string{
		apianalytics.SubjectCallsAnalytics,
		apianalytics.SubjectOperatorStateAnalytics,
	})
	ensureStream(t, natsURL, "RECORDING", []string{"tenant.>"})

	dsns := startCH(t)
	migrateUp(t, dsns.migrate)
	conn, err := store.Open(ctx, store.Config{
		DSN:           dsns.verify,
		BatchSize:     100,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	tenantID := uuid.New()
	projectID := uuid.New()
	eventID := uuid.New()

	mkPayload := func() []byte {
		ev := apianalytics.AnalyticsCallEventPayload{
			Date:        "2026-05-14",
			TS:          time.Now().UTC(),
			TenantID:    tenantID,
			ProjectID:   projectID,
			OperatorID:  uuid.New(),
			CallID:      uuid.New(),
			Status:      "success",
			DurationSec: 60,
			HangupCause: "NORMAL_CLEARING",
			RegionCode:  "MSK",
			AttemptNo:   1,
			TrunkUsed:   "trunk-a",
			EventID:     eventID, // same event_id every time
		}
		raw, err := json.Marshal(ev)
		require.NoError(t, err)
		return raw
	}

	runOne := func(runCtx context.Context, reg *prometheus.Registry, expected uint64) {
		// Per-invocation: a fresh NATSSubscriber so its consumer is
		// torn down with its context, and a fresh LRU inside the
		// pipeline.
		runCtx, runCancel := context.WithCancel(runCtx)
		defer runCancel()

		sub, err := eventbus.NewNATSSubscriber(runCtx, []string{natsURL}, "")
		require.NoError(t, err)

		m, err := metrics.RegisterIngestMetrics(reg)
		require.NoError(t, err)

		p, err := service.NewIngestPipeline(
			sub,
			&service.StoreAdapter{Conn: conn},
			zap.NewNop(),
			m,
			service.IngestConfig{
				BatchSize:     1,
				FlushInterval: 200 * time.Millisecond,
				DedupSize:     1000,
			},
		)
		require.NoError(t, err)

		runErrCh := make(chan error, 1)
		go func() { runErrCh <- p.Run(runCtx) }()

		// Allow handler registration.
		time.Sleep(200 * time.Millisecond)

		require.NoError(t, pub.Publish(runCtx, apianalytics.SubjectCallsAnalytics, mkPayload()))
		awaitCount(t, 5*time.Second, expected, func() uint64 {
			return chRowCountWithCtx(runCtx, t, conn, "events_calls", tenantID)
		})

		// Assert the miss counter ticked at least once on the
		// cold-LRU add.
		require.Eventually(t,
			func() bool { return gatherDedupMiss(t, reg) >= 1 },
			3*time.Second, 50*time.Millisecond,
			"dedup_miss_total must tick when LRU is cold and a new event_id is added",
		)

		runCancel()
		_ = sub.Close()
		require.ErrorIs(t, <-runErrCh, context.Canceled)
	}

	reg1 := prometheus.NewRegistry()
	runOne(ctx, reg1, 1)

	// Second invocation — same event_id, FRESH LRU (separate pipeline,
	// fresh DedupLRU). The CH row count would be 2 without dedup; FINAL
	// brings it back to 1 below. The miss counter on the fresh registry
	// reflects ONLY this run's cold-start adds.
	reg2 := prometheus.NewRegistry()
	runOne(ctx, reg2, 2)

	// ReplacingMergeTree reconciles asynchronously; force the merge to
	// assert the storage-level dedup invariant.
	require.NoError(t, conn.Driver().Exec(ctx, "OPTIMIZE TABLE events_calls FINAL"))
	require.Equal(t, uint64(1), chRowCountFINAL(t, conn, "events_calls", tenantID),
		"after OPTIMIZE FINAL the duplicate (same event_id, two cold-LRU adds) must collapse to 1 row")
}

// gatherDedupMiss returns the value of
// sociopulse_analytics_ingest_dedup_miss_total filtered to the calls
// subject. Mirrors counterValueOrZero in ingest_test.go but takes a
// generic Gatherer.
func gatherDedupMiss(t *testing.T, g prometheus.Gatherer) uint64 {
	t.Helper()
	families, err := g.Gather()
	require.NoError(t, err)
	const wantName = "sociopulse_analytics_ingest_dedup_miss_total"
	for _, fam := range families {
		if fam.GetName() != wantName {
			continue
		}
		for _, mp := range fam.GetMetric() {
			for _, lp := range mp.GetLabel() {
				if lp.GetName() == "subject" && lp.GetValue() == apianalytics.SubjectCallsAnalytics {
					return uint64(mp.GetCounter().GetValue())
				}
			}
		}
	}
	return 0
}
