package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/config"
	"github.com/mochizuki875/llm-file-gateway/internal/files"
	"github.com/mochizuki875/llm-file-gateway/internal/store"
)

// Server holds the dependencies shared by all HTTP handlers.
type Server struct {
	settings config.Config
	store    *store.Store
	files    *files.Service
	client   *http.Client

	// resolver resolves hostnames for file URL SSRF validation.
	resolver ipResolver
	// fileDialer pins file URL connections to validated public IPs.
	fileDialer *publicDialer
	// fileDownloadClient is the shared HTTP client used for file URL
	// downloads. It is created once so that connections are reused across
	// requests instead of being opened and closed for every download.
	fileDownloadClient *http.Client
}

// NewHandler builds the HTTP handler that exposes the health, Files, Responses,
// Chat Completions, and passthrough endpoints.
func NewHandler(settings config.Config, dataStore *store.Store, fileService *files.Service) http.Handler {
	server := &Server{
		settings: settings, store: dataStore, files: fileService,
		client: &http.Client{Timeout: settings.RequestTimeout},
	}
	server.resolver = net.DefaultResolver
	server.fileDialer = &publicDialer{
		resolver: server.resolver,
		dial:     (&net.Dialer{Timeout: settings.RequestTimeout}).DialContext,
	}
	server.fileDownloadClient = &http.Client{
		Timeout: settings.RequestTimeout,
		Transport: &http.Transport{
			DialContext: server.fileDialer.DialContext,
		},
	}
	server.fileDownloadClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", server.health)
	mux.HandleFunc("POST /v1/files", server.createFile)
	mux.HandleFunc("GET /v1/files", server.listFiles)
	mux.HandleFunc("GET /v1/files/{file_id}", server.retrieveFile)
	mux.HandleFunc("GET /v1/files/{file_id}/content", server.retrieveContent)
	mux.HandleFunc("DELETE /v1/files/{file_id}", server.deleteFile)
	mux.HandleFunc("POST /v1/responses", server.responses)
	mux.HandleFunc("POST /v1/chat/completions", server.chatCompletions)
	mux.HandleFunc("/v1/{path...}", server.passthrough)
	return mux
}

// health reports that the process is running. It does not check connectivity
// to SQLite, LibreOffice, or vLLM.
func (server *Server) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

// uploadBodyLimit returns the maximum HTTP request body size for file
// uploads. It is the per-file limit plus a fixed allowance for multipart
// overhead; a limit of 0 means unlimited.
func (server *Server) uploadBodyLimit() int64 {
	if server.settings.MaxFileBytes <= 0 {
		return 0
	}
	return server.settings.MaxFileBytes + 1<<20
}

// createFile handles POST /v1/files: it validates the multipart upload and
// queues the file for asynchronous conversion.
func (server *Server) createFile(response http.ResponseWriter, request *http.Request) {
	tenantID, gatewayError := server.tenantID(request)
	if gatewayError != nil {
		writeError(response, gatewayError)
		return
	}
	if limit := server.uploadBodyLimit(); limit > 0 {
		request.Body = http.MaxBytesReader(response, request.Body, limit)
	}
	input, header, err := request.FormFile("file")
	if err != nil {
		writeError(response, apierror.New(400, "invalid_request", "file is required.", "file"))
		return
	}
	defer func() { _ = input.Close() }()
	purpose := request.FormValue("purpose")
	if purpose == "" {
		writeError(response, apierror.New(400, "invalid_request", "purpose is required.", "purpose"))
		return
	}
	if !validFilePurpose(purpose) {
		writeError(response, apierror.New(400, "invalid_request", "Invalid purpose.", "purpose"))
		return
	}
	ttl := server.settings.FileTTL
	var expiresAfter struct {
		Anchor  string `json:"anchor"`
		Seconds int64  `json:"seconds"`
	}
	expiresProvided := false
	if expires := request.FormValue("expires_after"); expires != "" {
		expiresProvided = true
		if json.Unmarshal([]byte(expires), &expiresAfter) != nil {
			expiresAfter = struct {
				Anchor  string `json:"anchor"`
				Seconds int64  `json:"seconds"`
			}{}
		}
	} else if anchor := request.FormValue("expires_after[anchor]"); anchor != "" || request.FormValue("expires_after[seconds]") != "" {
		expiresProvided = true
		expiresAfter.Anchor = anchor
		expiresAfter.Seconds, _ = strconv.ParseInt(request.FormValue("expires_after[seconds]"), 10, 64)
	}
	if expiresProvided {
		maxSeconds := int64(server.settings.FileTTL / time.Second)
		if expiresAfter.Anchor != "created_at" || expiresAfter.Seconds < 1 || (maxSeconds > 0 && expiresAfter.Seconds > maxSeconds) {
			message := "expires_after must use anchor=created_at and seconds between 1 and "
			if maxSeconds > 0 {
				message += fmt.Sprintf("%d.", maxSeconds)
			} else {
				message += "unlimited."
			}
			writeError(response, apierror.New(400, "invalid_expires_after", message, "expires_after"))
			return
		}
		ttl = time.Duration(expiresAfter.Seconds) * time.Second
	}
	record, err := server.files.Create(request.Context(), header.Filename, input, purpose, tenantID, ttl)
	if err != nil {
		writeAnyError(response, request, err)
		return
	}
	writeJSON(response, http.StatusOK, openAIFile(*record))
}

// listFiles handles GET /v1/files with limit, order, purpose, and after
// pagination parameters.
func (server *Server) listFiles(response http.ResponseWriter, request *http.Request) {
	tenantID, gatewayError := server.tenantID(request)
	if gatewayError != nil {
		writeError(response, gatewayError)
		return
	}
	limit := 20
	var err error
	if raw := request.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 10_000 {
			writeError(response, apierror.New(422, "invalid_request", "limit must be between 1 and 10000.", "limit"))
			return
		}
	}
	order := request.URL.Query().Get("order")
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		writeError(response, apierror.New(422, "invalid_request", "order must be asc or desc.", "order"))
		return
	}
	purpose := request.URL.Query().Get("purpose")
	records, err := server.store.List(request.Context(), tenantID, purpose, limit+1, order, request.URL.Query().Get("after"))
	if err != nil {
		writeAnyError(response, request, err)
		return
	}
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	data := make([]map[string]any, 0, len(records))
	for _, record := range records {
		data = append(data, openAIFile(record))
	}
	var firstID, lastID any
	if len(records) > 0 {
		firstID, lastID = records[0].ID, records[len(records)-1].ID
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"object": "list", "data": data, "first_id": firstID, "last_id": lastID, "has_more": hasMore,
	})
}

// validFilePurpose reports whether purpose is one of the purposes accepted by
// the OpenAI Files Create API. Output purposes (assistants_output,
// batch_output, fine-tune-results) are assigned by the API to generated files
// and are not valid create inputs.
func validFilePurpose(purpose string) bool {
	switch purpose {
	case "assistants", "batch", "fine-tune", "vision", "user_data", "evals":
		return true
	default:
		return false
	}
}

// retrieveFile handles GET /v1/files/{file_id}.
func (server *Server) retrieveFile(response http.ResponseWriter, request *http.Request) {
	if record := server.ownedFile(response, request); record != nil {
		writeJSON(response, http.StatusOK, openAIFile(*record))
	}
}

// retrieveContent handles GET /v1/files/{file_id}/content and streams the
// original uploaded file. It holds a read lease on the file artifacts so that
// a concurrent DELETE or janitor cleanup waits until the download completes.
func (server *Server) retrieveContent(response http.ResponseWriter, request *http.Request) {
	tenantID, gatewayError := server.tenantID(request)
	if gatewayError != nil {
		writeError(response, gatewayError)
		return
	}
	id := request.PathValue("file_id")
	record, input, release, err := server.files.OpenSource(request.Context(), id, tenantID)
	if err != nil {
		writeAnyError(response, request, err)
		return
	}
	defer release()
	defer func() { _ = input.Close() }()
	response.Header().Set("Content-Type", record.MediaType)
	response.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", record.Filename))
	http.ServeContent(response, request, record.Filename, time.Unix(record.CreatedAt, 0), input)
}

// deleteFile handles DELETE /v1/files/{file_id}.
func (server *Server) deleteFile(response http.ResponseWriter, request *http.Request) {
	tenantID, gatewayError := server.tenantID(request)
	if gatewayError != nil {
		writeError(response, gatewayError)
		return
	}
	id := request.PathValue("file_id")
	deleted, err := server.files.Delete(request.Context(), id, tenantID)
	if err != nil {
		writeAnyError(response, request, err)
		return
	}
	if !deleted {
		writeError(response, apierror.New(404, "file_not_found", "File not found.", "file_id"))
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"id": id, "object": "file", "deleted": true})
}

// ownedFile resolves the file_id path parameter to a file owned by the
// authenticated tenant, writing an error response and returning nil when the
// file does not exist.
func (server *Server) ownedFile(response http.ResponseWriter, request *http.Request) *store.File {
	tenantID, gatewayError := server.tenantID(request)
	if gatewayError != nil {
		writeError(response, gatewayError)
		return nil
	}
	record, err := server.store.Get(request.Context(), request.PathValue("file_id"), tenantID)
	if err != nil {
		writeAnyError(response, request, err)
		return nil
	}
	if record == nil {
		slog.Warn("file not found", "file_id", request.PathValue("file_id"), "param", "file_id")
		writeError(response, apierror.New(404, "file_not_found", "File not found.", "file_id"))
		return nil
	}
	return record
}

// tenantID resolves the tenant for a request. When authentication is disabled
// it returns the shared tenant; otherwise it validates the bearer token and
// derives a tenant ID from its SHA-256 hash.
func (server *Server) tenantID(request *http.Request) (string, *apierror.Error) {
	authorization := request.Header.Get("Authorization")
	if !server.settings.GatewayAuthRequired {
		return store.SharedTenantID, nil
	}
	if !strings.HasPrefix(authorization, "Bearer ") {
		return "", apierror.New(401, "invalid_api_key", "Missing bearer token.", "")
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(server.settings.GatewayAPIKey)) != 1 {
		return "", apierror.New(401, "invalid_api_key", "Invalid API key.", "")
	}
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])[:32], nil
}

// openAIFile converts a store.File into the OpenAI Files API JSON shape.
func openAIFile(record store.File) map[string]any {
	status := "uploaded"
	switch record.Status {
	case "processed":
		status = "processed"
	case "failed":
		status = "error"
	}
	var expiresAt any
	if record.ExpiresAt > 0 {
		expiresAt = record.ExpiresAt
	}
	var statusDetails any
	if record.Status == "failed" && record.ErrorMessage.Valid && record.ErrorMessage.String != "" {
		statusDetails = record.ErrorMessage.String
	}
	return map[string]any{
		"id": record.ID, "object": "file", "bytes": record.Bytes,
		"created_at": record.CreatedAt, "expires_at": expiresAt,
		"filename": record.Filename, "purpose": record.Purpose, "status": status,
		"status_details": statusDetails,
	}
}

// writeAnyError writes a gateway error when err is one, otherwise it logs the
// unexpected error and writes a generic 500 response.
func writeAnyError(response http.ResponseWriter, request *http.Request, err error) {
	var gatewayError *apierror.Error
	if errors.As(err, &gatewayError) {
		writeError(response, gatewayError)
		return
	}
	slog.Error("request failed", "method", request.Method, "path", request.URL.Path, "error", err)
	writeError(response, apierror.New(500, "internal_error", "Internal server error.", ""))
}

// writeError writes an OpenAI-format error response.
func writeError(response http.ResponseWriter, gatewayError *apierror.Error) {
	var param any
	if gatewayError.Param != "" {
		param = gatewayError.Param
	}
	if gatewayError.Status >= http.StatusBadRequest && gatewayError.Status < http.StatusInternalServerError {
		slog.Warn("request rejected", "status", gatewayError.Status, "code", gatewayError.Code, "param", param)
	}
	writeJSON(response, gatewayError.Status, map[string]any{"error": map[string]any{
		"message": gatewayError.Message, "type": "invalid_request_error", "param": param, "code": gatewayError.Code,
	}})
}

// writeJSON writes value as a JSON response with the given status code.
func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		slog.Debug("response write interrupted", "status", status, "error", err)
	}
}
