package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
)

// fakeEnqueuer captures asynq.Client.Enqueue calls without round-tripping
// through Redis. Tests inspect the recorded payload and (optionally)
// inject the configured error path.
type fakeEnqueuer struct {
	mu sync.Mutex

	tasks []*asynq.Task
	opts  [][]asynq.Option
	err   error
}

func (e *fakeEnqueuer) Enqueue(t *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		err := e.err
		e.err = nil
		return nil, err
	}
	e.tasks = append(e.tasks, t)
	e.opts = append(e.opts, opts)
	return &asynq.TaskInfo{ID: uuid.NewString(), Queue: "crm"}, nil
}

func (e *fakeEnqueuer) snapshot() []*asynq.Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*asynq.Task, len(e.tasks))
	copy(out, e.tasks)
	return out
}

// fakeProgressTracker records the calls without speaking to Redis.
type fakeProgressTracker struct {
	mu sync.Mutex

	initCalls   int
	updateCalls int
	finishCalls int
	failCalls   int

	totals []int

	lastFailMsg string
	lastFinish  struct {
		total, inserted, skipped int
	}

	statuses  map[string]*crmapi.ImportStatus
	statusErr error
}

func newFakeProgress() *fakeProgressTracker {
	return &fakeProgressTracker{statuses: map[string]*crmapi.ImportStatus{}}
}

func (p *fakeProgressTracker) Init(_ context.Context, jobID string, _ uuid.UUID, total int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initCalls++
	p.totals = append(p.totals, total)
	p.statuses[jobID] = &crmapi.ImportStatus{JobID: jobID, State: "running", Total: total}
	return nil
}

func (p *fakeProgressTracker) Update(_ context.Context, jobID string, _ uuid.UUID, processed, inserted, skipped int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updateCalls++
	if st, ok := p.statuses[jobID]; ok {
		st.Processed = processed
		st.Inserted = inserted
		st.Skipped = skipped
	}
	return nil
}

func (p *fakeProgressTracker) Finish(_ context.Context, jobID string, _ uuid.UUID, total, inserted, skipped int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishCalls++
	p.lastFinish.total = total
	p.lastFinish.inserted = inserted
	p.lastFinish.skipped = skipped
	if st, ok := p.statuses[jobID]; ok {
		st.State = "succeeded"
		st.Total = total
		st.Inserted = inserted
		st.Skipped = skipped
	}
	return nil
}

func (p *fakeProgressTracker) Fail(_ context.Context, jobID string, _ uuid.UUID, msg string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failCalls++
	p.lastFailMsg = msg
	if st, ok := p.statuses[jobID]; ok {
		st.State = "failed"
	}
	return nil
}

func (p *fakeProgressTracker) Status(_ context.Context, jobID string) (*crmapi.ImportStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.statusErr != nil {
		err := p.statusErr
		p.statusErr = nil
		return nil, err
	}
	st, ok := p.statuses[jobID]
	if !ok {
		return nil, crmapi.ErrImportNotFound
	}
	return st, nil
}

// importTestFixture wires a RespondentService with import-path
// dependencies stubbed by hand. Returned references let each test
// inspect what the service did.
type importTestFixture struct {
	svc    *RespondentService
	tx     *fakeRespondentTxRunner
	store  *fakeRespondentStore
	kms    *fakeKMS
	hasher *fakePhoneHasher
	audit  *fakeRespondentAudit
	enq    *fakeEnqueuer
	prog   *fakeProgressTracker
}

func newImportFixture(t *testing.T) *importTestFixture {
	t.Helper()
	svc, tx, store, kms, hasher, audit := newRespSvc(t)
	enq := &fakeEnqueuer{}
	prog := newFakeProgress()
	svc = svc.WithEnqueuer(enq).WithProgress(prog)
	return &importTestFixture{
		svc:    svc,
		tx:     tx,
		store:  store,
		kms:    kms,
		hasher: hasher,
		audit:  audit,
		enq:    enq,
		prog:   prog,
	}
}

// =========================
// CSV parser tests
// =========================

func TestParseCSV_FiveRowsHappyPath(t *testing.T) {
	t.Parallel()

	csvBody := strings.Join([]string{
		"phone,full_name",
		"+79161234567,Иванов Иван",
		"+79161234568,Петров Пётр",
		"+79161234569,Сидоров Сидор",
		"+79161234570,Кузнецов Кузьма",
		"+79161234571,Smith John",
	}, "\n")
	rows, err := parseCSV(strings.NewReader(csvBody))
	require.NoError(t, err)
	require.Len(t, rows, 5)
	require.Equal(t, "+79161234567", rows[0].Phone)
	require.Equal(t, "Иванов Иван", rows[0].FullName)
	require.Equal(t, 2, rows[0].LineNumber, "first data row is line 2 (header is line 1)")
	require.Equal(t, 6, rows[4].LineNumber)
}

func TestParseCSV_StripsBOM(t *testing.T) {
	t.Parallel()

	bomBody := append([]byte{0xEF, 0xBB, 0xBF}, []byte("phone\n+79161234567\n")...)
	rows, err := parseCSV(bytes.NewReader(bomBody))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "+79161234567", rows[0].Phone)
}

func TestParseCSV_AcceptsCRLFLineEndings(t *testing.T) {
	t.Parallel()

	body := "phone\r\n+79161234567\r\n+79161234568\r\n"
	rows, err := parseCSV(strings.NewReader(body))
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestParseCSV_RejectsMissingPhoneHeader(t *testing.T) {
	t.Parallel()

	body := "name\nJohn\n"
	_, err := parseCSV(strings.NewReader(body))
	require.Error(t, err)
	require.Contains(t, err.Error(), "phone column")
}

func TestParseCSV_AcceptsRussianHeader(t *testing.T) {
	t.Parallel()

	body := "Телефон,ФИО\n+79161234567,Иванов\n"
	rows, err := parseCSV(strings.NewReader(body))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "+79161234567", rows[0].Phone)
	require.Equal(t, "Иванов", rows[0].FullName)
}

func TestParseCSV_StuffsUnknownColumnsIntoAttributes(t *testing.T) {
	t.Parallel()

	body := "phone,age,city\n+79161234567,42,Москва\n"
	rows, err := parseCSV(strings.NewReader(body))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "42", rows[0].Attributes["age"])
	require.Equal(t, "Москва", rows[0].Attributes["city"])
}

func TestParseCSV_SkipsBlankRows(t *testing.T) {
	t.Parallel()

	body := "phone\n+79161234567\n\n+79161234568\n"
	rows, err := parseCSV(strings.NewReader(body))
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestParseCSV_EmptyFile(t *testing.T) {
	t.Parallel()

	_, err := parseCSV(strings.NewReader(""))
	require.Error(t, err)
}

// =========================
// XLSX parser tests
// =========================

// buildXLSXFile builds an in-memory xlsx workbook from the supplied
// 2D string content. Row 0 is the header.
func buildXLSXFile(t *testing.T, sheet string, data [][]string) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	if sheet != "Sheet1" {
		idx, err := f.NewSheet(sheet)
		require.NoError(t, err)
		f.SetActiveSheet(idx)
	}
	for ri, row := range data {
		for ci, cell := range row {
			ax, err := excelize.CoordinatesToCellName(ci+1, ri+1)
			require.NoError(t, err)
			require.NoError(t, f.SetCellValue(sheet, ax, cell))
		}
	}
	var buf bytes.Buffer
	require.NoError(t, f.Write(&buf))
	return buf.Bytes()
}

func TestParseXLSX_FiveRowsHappyPath(t *testing.T) {
	t.Parallel()

	body := buildXLSXFile(t, "Sheet1", [][]string{
		{"phone", "full_name"},
		{"+79161234567", "Иванов"},
		{"+79161234568", "Петров"},
		{"+79161234569", "Сидоров"},
		{"+79161234570", "Кузнецов"},
		{"+79161234571", "Smith"},
	})
	rows, err := parseXLSX(bytes.NewReader(body))
	require.NoError(t, err)
	require.Len(t, rows, 5)
	require.Equal(t, "+79161234567", rows[0].Phone)
	require.Equal(t, "Иванов", rows[0].FullName)
}

func TestParseXLSX_RejectsMissingPhoneHeader(t *testing.T) {
	t.Parallel()

	body := buildXLSXFile(t, "Sheet1", [][]string{
		{"name"},
		{"John"},
	})
	_, err := parseXLSX(bytes.NewReader(body))
	require.Error(t, err)
}

func TestParseXLSX_NormalisesPhoneCellInScientificNotation(t *testing.T) {
	t.Parallel()

	// Simulate the case where Excel autoformats a long numeric phone
	// cell as scientific notation. Inserting via SetCellValue with a
	// big int forces excelize to materialise as 'general' format,
	// which on subsequent read can come back as scientific.
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	require.NoError(t, f.SetCellValue("Sheet1", "A1", "phone"))
	// Use SetCellFloat so the cell is stored as a number.
	require.NoError(t, f.SetCellFloat("Sheet1", "A2", 89161234567, -1, 64))
	var buf bytes.Buffer
	require.NoError(t, f.Write(&buf))

	rows, err := parseXLSX(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// Phone repaired into a digit string the downstream normaliser
	// can parse without scientific notation upsetting it.
	got := rows[0].Phone
	require.NotContains(t, got, "e", "phone cell with float repair must drop scientific notation")
}

// =========================
// Import handler tests
// =========================

func TestImport_RejectsNilTenantID(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	_, err := fx.svc.Import(context.Background(), crmapi.ImportRequest{
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
		Body:      []byte("phone\n+79161234567\n"),
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

func TestImport_RejectsUnsupportedFormat(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	_, err := fx.svc.Import(context.Background(), crmapi.ImportRequest{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    "json",
		Body:      []byte("{}"),
	})
	require.ErrorIs(t, err, crmapi.ErrImportFormatUnsupported)
}

func TestImport_RejectsEmptyBody(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	_, err := fx.svc.Import(context.Background(), crmapi.ImportRequest{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

func TestImport_HappyPathEnqueuesAndInits(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	tk, err := fx.svc.Import(context.Background(), crmapi.ImportRequest{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
		Body:      []byte("phone\n+79161234567\n"),
	})
	require.NoError(t, err)
	require.NotNil(t, tk)
	require.NotEmpty(t, tk.JobID)
	require.True(t, tk.Enqueued)
	require.Equal(t, "queued", tk.Status)

	require.Equal(t, 1, fx.prog.initCalls, "Init should be called pre-enqueue")
	require.Len(t, fx.enq.snapshot(), 1)

	// Audit row recorded.
	events := fx.audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.respondents.import.queued", events[0].Action)
}

func TestImport_DuplicateTaskIDIsIdempotent(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	fx.enq.err = asynq.ErrDuplicateTask
	tk, err := fx.svc.Import(context.Background(), crmapi.ImportRequest{
		JobID:     "fixed-job-id",
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
		Body:      []byte("phone\n+79161234567\n"),
	})
	require.NoError(t, err)
	require.False(t, tk.Enqueued, "duplicate enqueue must surface as Enqueued=false")
	require.Equal(t, "fixed-job-id", tk.JobID)
}

func TestImport_EnqueueFailureFailsTracker(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	fx.enq.err = errors.New("redis down")
	_, err := fx.svc.Import(context.Background(), crmapi.ImportRequest{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
		Body:      []byte("phone\n+79161234567\n"),
	})
	require.Error(t, err)
	require.Equal(t, 1, fx.prog.failCalls, "Fail should be invoked when Enqueue errors")
}

func TestHandleImportTask_ParsesAndInserts(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	csvBody := "phone\n+79161234567\n+79161234568\n+79161234569\n"
	payload := importTaskPayload{
		JobID:     "job-1",
		TenantID:  tenantID,
		ProjectID: projectID,
		Format:    crmapi.ImportFormatCSV,
		Source:    crmapi.SourceImported,
		Body:      []byte(csvBody),
		StartedAt: time.Now(),
	}
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)

	task := asynq.NewTask(crmapi.TaskRespondentImport, encoded)
	require.NoError(t, fx.svc.HandleImportTask(context.Background(), task))

	require.Equal(t, 3, fx.prog.lastFinish.total)
	require.Equal(t, 3, fx.prog.lastFinish.inserted)
	require.Equal(t, 0, fx.prog.lastFinish.skipped)
	require.Len(t, fx.store.rows, 3)
}

func TestHandleImportTask_BadPhoneCountedAsSkipped(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	csvBody := "phone\n+79161234567\nbogus\n"
	payload := importTaskPayload{
		JobID:     "job-bad",
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
		Source:    crmapi.SourceImported,
		Body:      []byte(csvBody),
		StartedAt: time.Now(),
	}
	encoded, _ := json.Marshal(payload)
	task := asynq.NewTask(crmapi.TaskRespondentImport, encoded)
	require.NoError(t, fx.svc.HandleImportTask(context.Background(), task))

	require.Equal(t, 1, fx.prog.lastFinish.inserted)
	require.Equal(t, 1, fx.prog.lastFinish.skipped, "invalid phone counts as skipped, not error")
	require.Len(t, fx.store.rows, 1)
}

func TestHandleImportTask_DedupsWithinFile(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	csvBody := "phone\n+79161234567\n+79161234567\n"
	payload := importTaskPayload{
		JobID:     "job-dup",
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
		Source:    crmapi.SourceImported,
		Body:      []byte(csvBody),
		StartedAt: time.Now(),
	}
	encoded, _ := json.Marshal(payload)
	task := asynq.NewTask(crmapi.TaskRespondentImport, encoded)
	require.NoError(t, fx.svc.HandleImportTask(context.Background(), task))

	require.Equal(t, 1, fx.prog.lastFinish.inserted)
	require.Equal(t, 1, fx.prog.lastFinish.skipped)
	require.Len(t, fx.store.rows, 1)
}

func TestHandleImportTask_DedupesAgainstExisting(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	// Pre-seed the store with one row whose hash matches the file's
	// first row.
	preExistingHash, err := fx.hasher.Hash(context.Background(), tenantID, "+79161234567")
	require.NoError(t, err)
	key := respondentKey(tenantID, projectID, preExistingHash)
	fx.store.hashIndex[key] = uuid.New()
	fx.store.rows[fx.store.hashIndex[key]] = crmapi.Respondent{
		ID: fx.store.hashIndex[key], TenantID: tenantID, ProjectID: projectID, PhoneHash: preExistingHash,
	}

	csvBody := "phone\n+79161234567\n+79161234568\n"
	payload := importTaskPayload{
		JobID:     "job-existing",
		TenantID:  tenantID,
		ProjectID: projectID,
		Format:    crmapi.ImportFormatCSV,
		Source:    crmapi.SourceImported,
		Body:      []byte(csvBody),
		StartedAt: time.Now(),
	}
	encoded, _ := json.Marshal(payload)
	task := asynq.NewTask(crmapi.TaskRespondentImport, encoded)
	require.NoError(t, fx.svc.HandleImportTask(context.Background(), task))

	require.Equal(t, 1, fx.prog.lastFinish.inserted, "only the new phone should land")
	require.Equal(t, 1, fx.prog.lastFinish.skipped)
}

func TestHandleImportTask_DNCSkipped(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	// Seed DNC for the first phone hash.
	hash, err := fx.hasher.Hash(context.Background(), tenantID, "+79161234567")
	require.NoError(t, err)
	fx.store.dncBlocks[respondentKey(tenantID, projectID, hash)] = true

	csvBody := "phone\n+79161234567\n+79161234568\n"
	payload := importTaskPayload{
		JobID:     "job-dnc",
		TenantID:  tenantID,
		ProjectID: projectID,
		Format:    crmapi.ImportFormatCSV,
		Source:    crmapi.SourceImported,
		Body:      []byte(csvBody),
		StartedAt: time.Now(),
	}
	encoded, _ := json.Marshal(payload)
	task := asynq.NewTask(crmapi.TaskRespondentImport, encoded)
	require.NoError(t, fx.svc.HandleImportTask(context.Background(), task))

	require.Equal(t, 1, fx.prog.lastFinish.inserted)
	require.Equal(t, 1, fx.prog.lastFinish.skipped, "DNC row counts as skipped")
}

func TestHandleImportTask_ProgressUpdatesPerBatch(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	// Build a CSV with 2 batches of importBatchSize+ rows. We cap at
	// 2*importBatchSize+5 to confirm the batch loop emits one Update
	// per batch.
	const rowsCount = 2*importBatchSize + 5
	var sb strings.Builder
	sb.WriteString("phone\n")
	for i := 0; i < rowsCount; i++ {
		// Generate distinct valid phones.
		_, _ = fmt.Fprintf(&sb, "+7916%07d\n", 1234567+i)
	}
	payload := importTaskPayload{
		JobID:     "job-progress",
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
		Source:    crmapi.SourceImported,
		Body:      []byte(sb.String()),
		StartedAt: time.Now(),
	}
	encoded, _ := json.Marshal(payload)
	task := asynq.NewTask(crmapi.TaskRespondentImport, encoded)
	require.NoError(t, fx.svc.HandleImportTask(context.Background(), task))

	// Three batches → at least three Update calls.
	require.GreaterOrEqual(t, fx.prog.updateCalls, 3)
	require.Equal(t, 1, fx.prog.finishCalls)
}

func TestHandleImportTask_ContextCancellationFails(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	// Build a payload large enough that the loop iterates at least
	// twice, giving us a chance to cancel between batches.
	var sb strings.Builder
	sb.WriteString("phone\n")
	for i := 0; i < importBatchSize+10; i++ {
		_, _ = fmt.Fprintf(&sb, "+7916%07d\n", 2000000+i)
	}
	payload := importTaskPayload{
		JobID:     "job-cancel",
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
		Source:    crmapi.SourceImported,
		Body:      []byte(sb.String()),
		StartedAt: time.Now(),
	}
	encoded, _ := json.Marshal(payload)
	task := asynq.NewTask(crmapi.TaskRespondentImport, encoded)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel; the loop must surface ctx.Err() on first iteration
	err := fx.svc.HandleImportTask(ctx, task)
	require.Error(t, err)
	require.Equal(t, 1, fx.prog.failCalls)
}

func TestHandleImportTask_KMSEncryptFailureBubbles(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	fx.kms.encryptErr = errors.New("kms unavailable")
	csvBody := "phone\n+79161234567\n"
	payload := importTaskPayload{
		JobID:     "job-kms",
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Format:    crmapi.ImportFormatCSV,
		Source:    crmapi.SourceImported,
		Body:      []byte(csvBody),
		StartedAt: time.Now(),
	}
	encoded, _ := json.Marshal(payload)
	task := asynq.NewTask(crmapi.TaskRespondentImport, encoded)
	err := fx.svc.HandleImportTask(context.Background(), task)
	require.Error(t, err)
	require.Equal(t, 1, fx.prog.failCalls)
}

func TestGetImportStatus_PropagatesErrImportNotFound(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	_, err := fx.svc.GetImportStatus(context.Background(), "missing-job")
	require.ErrorIs(t, err, crmapi.ErrImportNotFound)
}

func TestGetImportStatus_RejectsBlankJobID(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	_, err := fx.svc.GetImportStatus(context.Background(), "   ")
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

func TestGetImportStatus_ReturnsRecordedStatus(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t)

	require.NoError(t, fx.prog.Init(context.Background(), "job-x", uuid.New(), 5))
	require.NoError(t, fx.prog.Update(context.Background(), "job-x", uuid.New(), 5, 4, 1))
	require.NoError(t, fx.prog.Finish(context.Background(), "job-x", uuid.New(), 5, 4, 1))

	st, err := fx.svc.GetImportStatus(context.Background(), "job-x")
	require.NoError(t, err)
	require.Equal(t, "succeeded", st.State)
	require.Equal(t, 5, st.Total)
	require.Equal(t, 4, st.Inserted)
	require.Equal(t, 1, st.Skipped)
}
