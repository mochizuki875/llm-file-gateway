package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/config"
	"github.com/mochizuki875/llm-file-gateway/internal/converter"
	"github.com/mochizuki875/llm-file-gateway/internal/logging"
	"github.com/mochizuki875/llm-file-gateway/internal/store"
)

// Service manages uploaded files: it stores them on disk, queues them for
// asynchronous conversion, and cleans up expired files.
type Service struct {
	settings   config.Config
	store      *store.Store
	dispatcher *converter.Dispatcher
	queue      chan conversionJob
	cancel     context.CancelFunc
	wait       sync.WaitGroup

	// lifecycleCtx is cancelled when the service stops. It is used for
	// conversion enqueueing so that a conversion job is not tied to the HTTP
	// request that created the file.
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	// lifecycleMu guards the conversions map of in-flight conversions so that
	// Delete and the janitor can wait for (or cancel) the conversion of a
	// specific file without serializing conversions of other files.
	lifecycleMu sync.Mutex
	conversions map[string]*fileLifecycle

	// leases tracks active read leases on file artifacts so that Delete and
	// the janitor do not remove the on-disk directory while inference is
	// reading it. It is guarded by lifecycleMu.
	leases map[string]*fileLease

	// deleting tracks files whose deletion has started (logically marked
	// deleted) but whose on-disk artifacts have not been removed yet. The
	// channel is closed when cleanup finishes. It is guarded by lifecycleMu
	// and prevents a new reader from acquiring a lease on a file that is
	// being deleted (TOCTOU between store lookup and lease acquisition).
	deleting map[string]chan struct{}

	// syncHandler is used for processing a single conversion job.
	// It is a field to allow injection for testing.
	syncHandler func(ctx context.Context, job conversionJob) error

	// afterPersist is invoked after the file record is persisted and before
	// the conversion job is enqueued. It is used by tests to simulate a
	// request context cancellation at the lifecycle boundary.
	afterPersist func()

	// retryCount tracks the number of retries per file ID.
	retryMu    sync.Mutex
	retryCount map[string]int
}

// fileLifecycle tracks a single in-flight conversion so that deletion can
// coordinate with it. done is closed when the conversion finishes.
type fileLifecycle struct {
	mu     sync.Mutex
	active bool
	done   chan struct{}
	cancel context.CancelFunc
}

// fileLease tracks active readers of a file's artifacts. Delete and the
// janitor wait for all readers to release the lease before removing the
// on-disk directory. done is closed when the last reader releases.
type fileLease struct {
	mu     sync.Mutex
	refs   int
	done   chan struct{}
	closed bool
}

// conversionJob is a single unit of work for the conversion workers.
type conversionJob struct {
	id                string
	documentConverter converter.DocumentConverter
}

// maxRetries is the number of times a conversion job will be retried before it is dropped.
const maxRetries = 3

// New creates a file service backed by the given store and converter dispatcher.
func New(settings config.Config, dataStore *store.Store, dispatcher *converter.Dispatcher) *Service {
	if dispatcher == nil {
		panic("converter dispatcher must not be nil")
	}
	service := &Service{settings: settings, store: dataStore, dispatcher: dispatcher, queue: make(chan conversionJob, 128), retryCount: make(map[string]int), conversions: make(map[string]*fileLifecycle), leases: make(map[string]*fileLease), deleting: make(map[string]chan struct{})}
	service.lifecycleCtx, service.lifecycleCancel = context.WithCancel(context.Background())
	service.syncHandler = service.convert
	return service
}

// lifecycle returns the service lifecycle context. It is cancelled by Stop so
// that conversion enqueueing is tied to the service lifetime rather than to
// any individual HTTP request.
func (service *Service) lifecycle() context.Context {
	return service.lifecycleCtx
}

// ResolveConverter returns the converter for the given path, falling back to
// the plain-text converter for unknown extensions.
func (service *Service) ResolveConverter(ctx context.Context, path string) (converter.DocumentConverter, error) {
	return service.dispatcher.ResolveConverter(ctx, path)
}

// Run starts the conversion workers and the janitor, then re-enqueues any
// files that were still pending from a previous run.
func (service *Service) Run(ctx context.Context, workers int) error {
	workerContext, cancel := context.WithCancel(ctx)
	service.cancel = cancel

	// Retrieve the list of pending files from the store.
	pending, err := service.store.Pending(ctx)
	if err != nil {
		return err
	}

	// Start the worker goroutines for file conversion.
	for i := 0; i < workers; i++ {
		service.wait.Go(func() { service.worker(workerContext) })
	}

	// Start the janitor goroutine for cleaning up expired files.
	service.wait.Go(func() { service.janitor(workerContext) })

	// Enqueue the pending files for conversion.
	for _, id := range pending {
		service.enqueue(service.lifecycle(), conversionJob{id: id})
	}

	slog.Info("file service started", "workers", workers, "pending_files", len(pending))
	return nil
}

// Stop cancels the service context and waits for all workers and the janitor
// to finish.
func (service *Service) Stop() {
	if service.cancel != nil {
		service.cancel()
	}
	if service.lifecycleCancel != nil {
		service.lifecycleCancel()
	}
	service.wait.Wait()
	slog.Info("file service stopped")
}

// ReconcileStorage removes file directories left by crashes before their
// database record was persisted. Directories referenced by any record,
// including failed or logically deleted records, are retained.
func (service *Service) ReconcileStorage(ctx context.Context) error {
	sourcePaths, err := service.store.SourcePaths(ctx)
	if err != nil {
		return err
	}
	referenced := make(map[string]bool, len(sourcePaths))
	for _, sourcePath := range sourcePaths {
		referenced[filepath.Clean(filepath.Dir(sourcePath))] = true
	}
	filesDir := filepath.Join(service.settings.DataDir, "files")
	info, err := os.Lstat(filesDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect files directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("files path must be a directory and not a symlink")
	}
	tenants, err := os.ReadDir(filesDir)
	if err != nil {
		return fmt.Errorf("read files directory: %w", err)
	}
	for _, tenant := range tenants {
		if tenant.Type()&os.ModeSymlink != 0 || !tenant.IsDir() {
			continue
		}
		tenantDir := filepath.Join(filesDir, tenant.Name())
		entries, err := os.ReadDir(tenantDir)
		if err != nil {
			return fmt.Errorf("read tenant file directory %q: %w", tenant.Name(), err)
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !managedFileID(entry.Name()) {
				continue
			}
			relativeDir := filepath.Clean(filepath.Join("files", tenant.Name(), entry.Name()))
			if referenced[relativeDir] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(tenantDir, entry.Name())); err != nil {
				return fmt.Errorf("remove orphan file directory %q: %w", relativeDir, err)
			}
			slog.Info("orphan file directory removed", "directory", relativeDir)
		}
	}
	return nil
}

func managedFileID(name string) bool {
	if len(name) != len("file_")+32 || !strings.HasPrefix(name, "file_") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(name, "file_"))
	return err == nil
}

// Create stores an uploaded file on disk, records it in the store, and queues
// it for asynchronous conversion. It returns the created file record.
func (service *Service) Create(ctx context.Context, filename string, source io.Reader, purpose, tenantID string, ttl time.Duration) (*store.File, error) {
	safeName := filepath.Base(filename)
	extension := strings.ToLower(filepath.Ext(safeName))
	documentConverter, err := service.dispatcher.ResolveConverter(ctx, safeName)
	if err != nil {
		return nil, apierror.New(400, "unsupported_file_type", "Unsupported file type.", "file")
	}
	id, err := newFileID()
	if err != nil {
		return nil, err
	}
	relativeDir := filepath.Join("files", tenantID, id)
	fileDir := filepath.Join(service.settings.DataDir, relativeDir)
	if err := os.MkdirAll(fileDir, 0o700); err != nil {
		return nil, err
	}
	keepFiles := false
	defer func() {
		if !keepFiles {
			if err := os.RemoveAll(fileDir); err != nil {
				slog.Warn("incomplete file cleanup failed", "file_id", id, "error", err)
			}
		}
	}()
	sourcePath := filepath.Join(fileDir, "source"+extension)
	output, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	var written int64
	var copyErr error
	if service.settings.MaxFileBytes > 0 {
		written, copyErr = io.Copy(io.MultiWriter(output, digest), io.LimitReader(source, service.settings.MaxFileBytes+1))
	} else {
		written, copyErr = io.Copy(io.MultiWriter(output, digest), source)
	}
	closeErr := output.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if service.settings.MaxFileBytes > 0 && written > service.settings.MaxFileBytes {
		return nil, apierror.FileTooLarge(service.settings.MaxFileBytes, "file")
	}
	if err := documentConverter.Validate(sourcePath); err != nil {
		if documentConverter.Extension() == "" {
			return nil, apierror.New(400, "unsupported_file_type", "unsupported file type", "file")
		}
		return nil, apierror.New(400, "unsupported_file_type", err.Error(), "file")
	}
	now := time.Now().Unix()
	expiresAt := now + int64(ttl/time.Second)
	if ttl <= 0 {
		// A non-positive TTL means the file never expires (expires_at = 0).
		expiresAt = 0
	}
	record := &store.File{
		ID: id, TenantID: tenantID, Filename: safeName, MediaType: documentConverter.MediaType(),
		Purpose: purpose, Bytes: written, SHA256: hex.EncodeToString(digest.Sum(nil)),
		Status: "uploaded", SourcePath: filepath.ToSlash(filepath.Join(relativeDir, "source"+extension)),
		CreatedAt: now, ExpiresAt: expiresAt,
	}
	if err := service.store.Add(ctx, *record); err != nil {
		return nil, err
	}
	keepFiles = true
	// The conversion lifecycle is decoupled from the HTTP request: once the
	// record is persisted, enqueueing uses the service lifecycle context so
	// that a client disconnect cannot prevent the conversion from starting.
	if service.afterPersist != nil {
		service.afterPersist()
	}
	service.enqueue(service.lifecycle(), conversionJob{id: id, documentConverter: documentConverter})
	slog.Info("file accepted", "file_id", id, "filename", safeName, "bytes", written)
	return record, nil
}

// Delete removes the file directory and the database record for the given
// file, reporting whether a file was actually deleted. It first marks the
// record as logically deleted so that the file disappears from the API and no
// new conversion can start, then waits for any in-flight conversion to finish
// before removing the on-disk artifacts and the database record.
func (service *Service) Delete(ctx context.Context, id, tenantID string) (bool, error) {
	record, err := service.store.Get(ctx, id, tenantID)
	if err != nil {
		return false, err
	}
	if record == nil {
		slog.Warn("file not found", "file_id", id, "param", "file_id")
		return false, nil
	}
	deletionDone, claimed := service.beginDeletion(id)
	if !claimed {
		return false, nil
	}
	defer service.endDeletion(id, deletionDone)
	marked, err := service.store.MarkDeleted(ctx, id, tenantID)
	if err != nil {
		return false, err
	}
	if !marked {
		return false, nil
	}
	service.waitForConversion(id)
	service.waitForLease(id)
	if err := os.RemoveAll(filepath.Dir(filepath.Join(service.settings.DataDir, record.SourcePath))); err != nil {
		return false, err
	}
	deleted, err := service.store.Delete(context.WithoutCancel(ctx), id, tenantID)
	if err != nil || !deleted {
		return deleted, err
	}
	slog.Info("file deleted", "file_id", id, "filename", record.Filename)
	return true, nil
}

// Resolve returns the file record and its conversion manifest for inference
// use. It rejects files that are still processing or that failed to convert.
// The returned release function must be called when the caller is done
// reading the file artifacts; until then Delete and the janitor wait before
// removing the on-disk directory.
func (service *Service) Resolve(ctx context.Context, id, tenantID, param string) (*store.File, converter.Manifest, func(), error) {
	record, err := service.store.Get(ctx, id, tenantID)
	if err != nil {
		return nil, converter.Manifest{}, nil, err
	}
	if record == nil {
		slog.Warn("file not found", "file_id", id, "param", param)
		return nil, converter.Manifest{}, nil, apierror.New(404, "file_not_found", "File not found.", param)
	}
	if record.Status == "uploaded" || record.Status == "processing" {
		return nil, converter.Manifest{}, nil, apierror.New(409, "file_not_ready", "The file is still being processed.", param)
	}
	if record.Status == "failed" || !record.ManifestPath.Valid {
		return nil, converter.Manifest{}, nil, apierror.New(422, "file_processing_failed", "File processing failed.", param)
	}
	// Acquire the lease before reading the manifest so that a concurrent
	// Delete or janitor cleanup cannot remove the artifacts between the
	// store lookup and the manifest read (TOCTOU).
	release, err := service.acquireLease(id)
	if err != nil {
		slog.Warn("file is being deleted", "file_id", id, "param", param)
		return nil, converter.Manifest{}, nil, apierror.New(404, "file_not_found", "File not found.", param)
	}
	content, err := os.ReadFile(filepath.Join(service.settings.DataDir, record.ManifestPath.String))
	if err != nil {
		release()
		slog.Error("file manifest read failed", "file_id", id, "error", err)
		return nil, converter.Manifest{}, nil, apierror.New(422, "file_processing_failed", "File manifest is missing.", param)
	}
	var manifest converter.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		release()
		slog.Error("file manifest decode failed", "file_id", id, "error", err)
		return nil, converter.Manifest{}, nil, err
	}
	return record, manifest, release, nil
}

// OpenSource opens the original uploaded file for reading while holding a
// read lease, so that Delete and the janitor wait until the caller is done
// before removing the on-disk directory. The caller must close the returned
// file and call the release function exactly once.
func (service *Service) OpenSource(ctx context.Context, id, tenantID string) (*store.File, *os.File, func(), error) {
	record, err := service.store.Get(ctx, id, tenantID)
	if err != nil {
		return nil, nil, nil, err
	}
	if record == nil {
		return nil, nil, nil, apierror.New(404, "file_not_found", "File not found.", "file_id")
	}
	release, err := service.acquireLease(id)
	if err != nil {
		return nil, nil, nil, apierror.New(404, "file_not_found", "File not found.", "file_id")
	}
	input, err := os.Open(filepath.Join(service.settings.DataDir, record.SourcePath))
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	return record, input, release, nil
}

// acquireLease registers a read lease on the given file's artifacts. It
// returns a release function that must be called exactly once. It fails when
// deletion of the file has already started so that a reader cannot start
// reading artifacts that are about to be removed.
func (service *Service) acquireLease(id string) (func(), error) {
	service.lifecycleMu.Lock()
	defer service.lifecycleMu.Unlock()
	if _, deleting := service.deleting[id]; deleting {
		return nil, errors.New("file is being deleted")
	}
	lease, ok := service.leases[id]
	if !ok {
		lease = &fileLease{done: make(chan struct{})}
		service.leases[id] = lease
	}
	lease.mu.Lock()
	lease.refs++
	lease.mu.Unlock()
	return func() {
		service.releaseLease(id, lease)
	}, nil
}

// releaseLease decrements the reference count of a lease and closes its done
// channel when the last reader releases it.
func (service *Service) releaseLease(id string, lease *fileLease) {
	service.lifecycleMu.Lock()
	lease.mu.Lock()
	lease.refs--
	if lease.refs == 0 && !lease.closed {
		lease.closed = true
		close(lease.done)
		delete(service.leases, id)
	}
	lease.mu.Unlock()
	service.lifecycleMu.Unlock()
}

// waitForLease waits until all readers of the given file have released their
// leases. It returns immediately when no reader holds a lease.
func (service *Service) waitForLease(id string) {
	service.lifecycleMu.Lock()
	lease, ok := service.leases[id]
	service.lifecycleMu.Unlock()
	if !ok {
		return
	}
	lease.mu.Lock()
	if lease.refs == 0 {
		lease.mu.Unlock()
		return
	}
	done := lease.done
	lease.mu.Unlock()
	<-done
}

// beginDeletion claims cleanup ownership for the given file and prevents new
// readers from acquiring a lease. A false return means another cleanup owns
// the file in this process.
func (service *Service) beginDeletion(id string) (chan struct{}, bool) {
	service.lifecycleMu.Lock()
	defer service.lifecycleMu.Unlock()
	if _, deleting := service.deleting[id]; deleting {
		return nil, false
	}
	done := make(chan struct{})
	service.deleting[id] = done
	return done, true
}

// endDeletion removes the deletion marker for the given file and signals any
// waiter that deletion has finished.
func (service *Service) endDeletion(id string, done chan struct{}) {
	service.lifecycleMu.Lock()
	if current, ok := service.deleting[id]; ok && current == done {
		delete(service.deleting, id)
	}
	service.lifecycleMu.Unlock()
	close(done)
}

// enqueue submits a conversion job to the worker queue, or drops it when the
// context is already cancelled.
func (service *Service) enqueue(ctx context.Context, job conversionJob) {
	select {
	case service.queue <- job:
		logging.V(ctx, 2, "file queued", "file_id", job.id)
	case <-ctx.Done():
	}
}

// enqueueAfter enqueues a conversion job after the provided amount of time.
func (service *Service) enqueueAfter(ctx context.Context, job conversionJob, after time.Duration) {
	service.retryMu.Lock()
	service.retryCount[job.id]++
	service.retryMu.Unlock()
	time.AfterFunc(after, func() {
		service.enqueue(ctx, job)
	})
}

// worker runs a worker thread that just dequeues jobs, processes them, and marks them done.
func (service *Service) worker(ctx context.Context) {
	for service.processNextJob(ctx) {
	}
}

// processNextJob dequeues a single conversion job and processes it.
// It returns false when the context is cancelled or the queue is closed.
func (service *Service) processNextJob(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case job, ok := <-service.queue:
		if !ok {
			return false
		}
		// Each conversion runs under a per-file context so that Delete and
		// the janitor can cancel a specific conversion without affecting
		// conversions of other files.
		fileCtx, cancel := context.WithCancel(ctx)
		state := service.beginConversion(job.id, cancel)
		err := service.syncHandler(fileCtx, job)
		service.endConversion(job.id, state)
		service.handleErr(ctx, err, job)
		return true
	}
}

// beginConversion registers an in-flight conversion for the given file and
// returns its lifecycle state. It is safe to call concurrently with Delete.
func (service *Service) beginConversion(id string, cancel context.CancelFunc) *fileLifecycle {
	state := &fileLifecycle{active: true, done: make(chan struct{}), cancel: cancel}
	service.lifecycleMu.Lock()
	service.conversions[id] = state
	service.lifecycleMu.Unlock()
	return state
}

// endConversion unregisters an in-flight conversion and signals any waiter.
func (service *Service) endConversion(id string, state *fileLifecycle) {
	service.lifecycleMu.Lock()
	if current, ok := service.conversions[id]; ok && current == state {
		delete(service.conversions, id)
	}
	service.lifecycleMu.Unlock()
	state.mu.Lock()
	state.active = false
	close(state.done)
	state.mu.Unlock()
}

// waitForConversion waits for any in-flight conversion of the given file to
// finish, cancelling it first. It returns immediately when no conversion is
// active. It never blocks conversions of other files.
func (service *Service) waitForConversion(id string) {
	service.lifecycleMu.Lock()
	state, ok := service.conversions[id]
	service.lifecycleMu.Unlock()
	if !ok {
		return
	}
	state.mu.Lock()
	if !state.active {
		state.mu.Unlock()
		return
	}
	state.cancel()
	state.mu.Unlock()
	<-state.done
}

// handleErr retries a failed conversion job with exponential backoff,
// up to maxRetries times, before dropping it out of the queue. Deterministic
// errors that cannot succeed on retry fail the file immediately.
func (service *Service) handleErr(ctx context.Context, err error, job conversionJob) {
	if err == nil {
		service.forget(job.id)
		return
	}
	if !converter.IsRetryable(err) {
		slog.Error("dropping conversion job out of the queue", "file_id", job.id, "error", err)
		service.markFailed(job.id, err)
		service.forget(job.id)
		return
	}
	if service.retries(job.id) >= maxRetries {
		slog.Error("dropping conversion job out of the queue", "file_id", job.id, "error", err)
		service.markFailed(job.id, err)
		service.forget(job.id)
		return
	}
	if !service.fileActive(job.id) {
		// The file was deleted while the conversion was failing; do not
		// schedule a retry that could resurrect it.
		service.forget(job.id)
		return
	}
	delay := backoffDelay(service.retries(job.id))
	slog.Warn("error converting file, retrying", "file_id", job.id, "error", err, "retry", service.retries(job.id)+1, "delay", delay)
	service.enqueueAfter(ctx, job, delay)
}

// fileActive reports whether the given file still exists and has not been
// logically deleted. It is used to avoid retrying conversions of deleted files.
func (service *Service) fileActive(id string) bool {
	record, err := service.store.GetInternal(context.Background(), id)
	if err != nil || record == nil || record.DeletedAt.Valid {
		return false
	}
	return true
}

func (service *Service) retries(id string) int {
	service.retryMu.Lock()
	defer service.retryMu.Unlock()
	return service.retryCount[id]
}

func (service *Service) forget(id string) {
	service.retryMu.Lock()
	defer service.retryMu.Unlock()
	delete(service.retryCount, id)
}

func backoffDelay(retry int) time.Duration {
	return time.Duration(1<<retry) * time.Second
}

// convert runs the actual document conversion for a job: it marks the file as
// processing, invokes the converter, and records the manifest path on success.
func (service *Service) convert(ctx context.Context, job conversionJob) error {
	record, err := service.store.GetInternal(ctx, job.id)
	if err != nil {
		return fmt.Errorf("queued file lookup: %w", err)
	}
	if record == nil {
		slog.Warn("queued file not found", "file_id", job.id)
		return nil
	}
	if record.DeletedAt.Valid {
		logging.V(ctx, 2, "skipping deleted queued file", "file_id", job.id)
		return nil
	}
	updated, err := service.store.UpdateStatus(ctx, job.id, "processing", "", "")
	if err != nil {
		return fmt.Errorf("document processing status update: %w", err)
	}
	if !updated {
		// The file was deleted (or logically deleted) between the lookup and
		// the status update. Do not start conversion and do not resurrect it.
		logging.V(ctx, 2, "skipping conversion of deleted file", "file_id", job.id)
		return nil
	}
	source := filepath.Join(service.settings.DataDir, record.SourcePath)
	documentConverter := job.documentConverter
	if documentConverter == nil {
		documentConverter, err = service.dispatcher.ResolveConverter(ctx, source)
		if err != nil {
			service.markFailed(job.id, err)
			return nil
		}
	}
	startedAt := time.Now()
	slog.Info("document conversion started", "file_id", job.id, "filename", record.Filename)
	_, err = documentConverter.Convert(ctx, source, filepath.Join(filepath.Dir(source), "derived"), converter.Options{
		MaxPages: service.settings.MaxDocumentPages, MaxTextChars: service.settings.MaxDocumentTextChars,
		DisableTextExtraction: !service.settings.TextExtractionEnabled,
	})
	if err != nil {
		return fmt.Errorf("document conversion: %w", err)
	}
	manifestPath := filepath.ToSlash(filepath.Join(filepath.Dir(record.SourcePath), "manifest.json"))
	updated, statusErr := service.store.UpdateStatus(context.Background(), job.id, "processed", manifestPath, "")
	if statusErr != nil {
		return fmt.Errorf("document conversion status update: %w", statusErr)
	}
	if !updated {
		// The file was deleted while conversion was running. The artifacts
		// were written but the record is gone (or logically deleted), so the
		// file must not be resurrected as processed.
		logging.V(ctx, 2, "skipping status update for deleted file", "file_id", job.id)
		return nil
	}
	slog.Info("document conversion completed", "file_id", job.id, "parts_manifest", manifestPath, "duration_ms", time.Since(startedAt).Milliseconds())
	return nil
}

// markFailed records a terminal conversion failure for the given file. It is a
// no-op when the file was deleted concurrently.
func (service *Service) markFailed(id string, err error) {
	updated, statusErr := service.store.UpdateStatus(context.Background(), id, "failed", "", truncate(err.Error(), 1000))
	if statusErr != nil {
		slog.Error("document failure status update failed", "file_id", id, "error", statusErr)
		return
	}
	if !updated {
		logging.V(context.Background(), 2, "skipping failure status for deleted file", "file_id", id)
	}
}

// janitor periodically deletes expired files until the context is cancelled.
func (service *Service) janitor(ctx context.Context) {
	service.deleteExpired(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			service.deleteExpired(ctx)
		}
	}
}

// deleteExpired removes the on-disk directories and database records of all
// expired files.
func (service *Service) deleteExpired(ctx context.Context) {
	expired, err := service.store.Expired(ctx)
	if err != nil {
		slog.Warn("expired file lookup failed", "error", err)
		return
	}
	for _, record := range expired {
		deletionDone, claimed := service.beginDeletion(record.ID)
		if !claimed {
			continue
		}
		if !record.DeletedAt.Valid {
			marked, err := service.store.MarkDeleted(ctx, record.ID, record.TenantID)
			if err != nil {
				service.endDeletion(record.ID, deletionDone)
				slog.Warn("expired file mark failed", "file_id", record.ID, "error", err)
				continue
			}
			if !marked {
				service.endDeletion(record.ID, deletionDone)
				continue
			}
		}
		service.waitForConversion(record.ID)
		service.waitForLease(record.ID)
		if err := os.RemoveAll(filepath.Dir(filepath.Join(service.settings.DataDir, record.SourcePath))); err != nil {
			service.endDeletion(record.ID, deletionDone)
			slog.Warn("expired file cleanup failed", "file_id", record.ID, "error", err)
			continue
		}
		deleted, err := service.store.Delete(context.WithoutCancel(ctx), record.ID, record.TenantID)
		service.endDeletion(record.ID, deletionDone)
		if err != nil {
			slog.Warn("expired file deletion failed", "file_id", record.ID, "error", err)
			continue
		}
		if deleted {
			slog.Debug("expired file deleted", "file_id", record.ID, "filename", record.Filename, "expires_at", record.ExpiresAt)
		}
	}
}

// newFileID generates a random file ID with a "file_" prefix.
func newFileID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate file ID: %w", err)
	}
	return "file_" + hex.EncodeToString(value), nil
}

// truncate shortens value to at most limit bytes.
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
