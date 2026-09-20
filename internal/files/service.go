package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

type Service struct {
	settings config.Config
	store    *store.Store
	queue    chan string
	cancel   context.CancelFunc
	wait     sync.WaitGroup
}

func New(settings config.Config, dataStore *store.Store) *Service {
	return &Service{settings: settings, store: dataStore, queue: make(chan string, 128)}
}

func (service *Service) Start(ctx context.Context) error {
	workerContext, cancel := context.WithCancel(ctx)
	service.cancel = cancel
	pending, err := service.store.Pending(ctx)
	if err != nil {
		return err
	}
	service.wait.Add(service.settings.ConversionWorkers + 1)
	for range service.settings.ConversionWorkers {
		go service.worker(workerContext)
	}
	go service.janitor(workerContext)
	for _, id := range pending {
		service.enqueue(workerContext, id)
	}
	slog.Info("file service started", "workers", service.settings.ConversionWorkers, "pending_files", len(pending))
	return nil
}

func (service *Service) Stop() {
	if service.cancel != nil {
		service.cancel()
	}
	service.wait.Wait()
	slog.Info("file service stopped")
}

func (service *Service) Create(ctx context.Context, filename string, source io.Reader, purpose, tenantID string) (*store.File, error) {
	safeName := filepath.Base(filename)
	extension := strings.ToLower(filepath.Ext(safeName))
	mediaType, err := converter.MediaType(extension)
	if err != nil {
		return nil, apierror.New(400, "unsupported_file_type", "Unsupported file type.", "file")
	}
	id, err := newFileID()
	if err != nil {
		return nil, err
	}
	relativeDir := filepath.Join("files", tenantID, id)
	fileDir := filepath.Join(service.settings.DataDir, relativeDir)
	if err := os.MkdirAll(fileDir, 0o755); err != nil {
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
	written, copyErr := io.Copy(io.MultiWriter(output, digest), io.LimitReader(source, service.settings.MaxFileBytes+1))
	closeErr := output.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if written > service.settings.MaxFileBytes {
		return nil, apierror.FileTooLarge(service.settings.MaxFileBytes, "file")
	}
	if err := converter.Validate(sourcePath); err != nil {
		return nil, apierror.New(400, "unsupported_file_type", err.Error(), "file")
	}
	now := time.Now().Unix()
	record := &store.File{
		ID: id, TenantID: tenantID, Filename: safeName, MediaType: mediaType,
		Purpose: purpose, Bytes: written, SHA256: hex.EncodeToString(digest.Sum(nil)),
		Status: "uploaded", SourcePath: filepath.ToSlash(filepath.Join(relativeDir, "source"+extension)),
		CreatedAt: now, ExpiresAt: now + int64(service.settings.FileTTL/time.Second),
	}
	if err := service.store.Add(ctx, *record); err != nil {
		return nil, err
	}
	keepFiles = true
	service.enqueue(ctx, id)
	slog.Info("file accepted", "file_id", id, "filename", safeName, "bytes", written)
	return record, nil
}

func (service *Service) Delete(ctx context.Context, id, tenantID string) (bool, error) {
	record, err := service.store.Get(ctx, id, tenantID)
	if err != nil {
		return false, err
	}
	if record == nil {
		slog.Warn("file not found", "file_id", id, "param", "file_id")
		return false, nil
	}
	if err := os.RemoveAll(filepath.Dir(filepath.Join(service.settings.DataDir, record.SourcePath))); err != nil {
		return false, err
	}
	deleted, err := service.store.Delete(ctx, id, tenantID)
	if err != nil || !deleted {
		return deleted, err
	}
	slog.Info("file deleted", "file_id", id, "filename", record.Filename)
	return true, nil
}

func (service *Service) Resolve(ctx context.Context, id, tenantID, param string) (*store.File, converter.Manifest, error) {
	record, err := service.store.Get(ctx, id, tenantID)
	if err != nil {
		return nil, converter.Manifest{}, err
	}
	if record == nil {
		slog.Warn("file not found", "file_id", id, "param", param)
		return nil, converter.Manifest{}, apierror.New(404, "file_not_found", "File not found.", param)
	}
	if record.Status == "uploaded" || record.Status == "processing" {
		return nil, converter.Manifest{}, apierror.New(409, "file_not_ready", "The file is still being processed.", param)
	}
	if record.Status == "failed" || !record.ManifestPath.Valid {
		return nil, converter.Manifest{}, apierror.New(422, "file_processing_failed", "File processing failed.", param)
	}
	content, err := os.ReadFile(filepath.Join(service.settings.DataDir, record.ManifestPath.String))
	if err != nil {
		slog.Error("file manifest read failed", "file_id", id, "error", err)
		return nil, converter.Manifest{}, apierror.New(422, "file_processing_failed", "File manifest is missing.", param)
	}
	var manifest converter.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		slog.Error("file manifest decode failed", "file_id", id, "error", err)
		return nil, converter.Manifest{}, err
	}
	return record, manifest, nil
}

func (service *Service) enqueue(ctx context.Context, id string) {
	select {
	case service.queue <- id:
		logging.V(ctx, 2, "file queued", "file_id", id)
	case <-ctx.Done():
	}
}

func (service *Service) worker(ctx context.Context) {
	defer service.wait.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-service.queue:
			service.convert(ctx, id)
		}
	}
}

func (service *Service) convert(ctx context.Context, id string) {
	record, err := service.store.GetInternal(ctx, id)
	if err != nil {
		slog.Error("queued file lookup failed", "file_id", id, "error", err)
		return
	}
	if record == nil {
		slog.Warn("queued file not found", "file_id", id)
		return
	}
	if record.DeletedAt.Valid {
		logging.V(ctx, 2, "skipping deleted queued file", "file_id", id)
		return
	}
	if err := service.store.UpdateStatus(ctx, id, "processing", "", ""); err != nil {
		slog.Error("document processing status update failed", "file_id", id, "error", err)
		return
	}
	source := filepath.Join(service.settings.DataDir, record.SourcePath)
	startedAt := time.Now()
	slog.Info("document conversion started", "file_id", id, "filename", record.Filename)
	_, err = converter.Convert(ctx, source, filepath.Join(filepath.Dir(source), "derived"), converter.Options{
		MaxPages: service.settings.MaxDocumentPages, MaxTextChars: service.settings.MaxDocumentTextChars,
		DisableTextExtraction: !service.settings.TextExtractionEnabled,
	})
	if err != nil {
		if statusErr := service.store.UpdateStatus(context.Background(), id, "failed", "", truncate(err.Error(), 1000)); statusErr != nil {
			slog.Error("document failure status update failed", "file_id", id, "error", statusErr)
		}
		slog.Error("document conversion failed", "file_id", id, "error", err)
		return
	}
	manifestPath := filepath.ToSlash(filepath.Join(filepath.Dir(record.SourcePath), "manifest.json"))
	if err := service.store.UpdateStatus(context.Background(), id, "processed", manifestPath, ""); err != nil {
		slog.Error("document conversion status update failed", "file_id", id, "error", err)
		return
	}
	slog.Info("document conversion completed", "file_id", id, "parts_manifest", manifestPath, "duration_ms", time.Since(startedAt).Milliseconds())
}

func (service *Service) janitor(ctx context.Context) {
	defer service.wait.Done()
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

func (service *Service) deleteExpired(ctx context.Context) {
	expired, err := service.store.Expired(ctx)
	if err != nil {
		slog.Warn("expired file lookup failed", "error", err)
		return
	}
	for _, record := range expired {
		if err := os.RemoveAll(filepath.Dir(filepath.Join(service.settings.DataDir, record.SourcePath))); err != nil {
			slog.Warn("expired file cleanup failed", "file_id", record.ID, "error", err)
			continue
		}
		if _, err := service.store.Delete(ctx, record.ID, record.TenantID); err != nil {
			slog.Warn("expired file deletion failed", "file_id", record.ID, "error", err)
		}
	}
}

func newFileID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate file ID: %w", err)
	}
	return "file_" + hex.EncodeToString(value), nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
