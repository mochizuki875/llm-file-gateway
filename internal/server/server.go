package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/config"
	"github.com/mochizuki875/llm-file-gateway/internal/files"
	"github.com/mochizuki875/llm-file-gateway/internal/store"
)

type Server struct {
	settings config.Config
	store    *store.Store
	files    *files.Service
	client   *http.Client
}

func NewHandler(settings config.Config, dataStore *store.Store, fileService *files.Service) http.Handler {
	server := &Server{
		settings: settings, store: dataStore, files: fileService,
		client: &http.Client{Timeout: settings.RequestTimeout},
	}
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

func (server *Server) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) createFile(response http.ResponseWriter, request *http.Request) {
	tenantID, gatewayError := server.tenantID(request)
	if gatewayError != nil {
		writeError(response, gatewayError)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, server.settings.MaxFileBytes+1<<20)
	input, header, err := request.FormFile("file")
	if err != nil {
		writeError(response, apierror.New(400, "invalid_request", "file is required.", "file"))
		return
	}
	defer input.Close()
	purpose := request.FormValue("purpose")
	if purpose == "" {
		purpose = "user_data"
	}
	if purpose != "user_data" {
		writeError(response, apierror.New(400, "invalid_purpose", "Only purpose=user_data is supported.", "purpose"))
		return
	}
	if expires := request.FormValue("expires_after"); expires != "" {
		var value struct {
			Anchor  string `json:"anchor"`
			Seconds int64  `json:"seconds"`
		}
		ttlSeconds := int64(server.settings.FileTTL / time.Second)
		if json.Unmarshal([]byte(expires), &value) != nil || value.Anchor != "created_at" || value.Seconds != ttlSeconds {
			message := fmt.Sprintf("Files expire %d seconds after creation.", ttlSeconds)
			writeError(response, apierror.New(400, "invalid_expires_after", message, "expires_after"))
			return
		}
	}
	record, err := server.files.Create(request.Context(), header.Filename, input, purpose, tenantID)
	if err != nil {
		writeAnyError(response, request, err)
		return
	}
	writeJSON(response, http.StatusOK, openAIFile(*record))
}

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
	records, err := server.store.List(request.Context(), tenantID, limit+1, order, request.URL.Query().Get("after"))
	if err != nil {
		writeAnyError(response, request, err)
		return
	}
	purpose := request.URL.Query().Get("purpose")
	filtered := make([]store.File, 0, len(records))
	for _, record := range records {
		if purpose == "" || purpose == record.Purpose {
			filtered = append(filtered, record)
		}
	}
	hasMore := len(filtered) > limit
	if hasMore {
		filtered = filtered[:limit]
	}
	data := make([]map[string]any, 0, len(filtered))
	for _, record := range filtered {
		data = append(data, openAIFile(record))
	}
	var firstID, lastID any
	if len(filtered) > 0 {
		firstID, lastID = filtered[0].ID, filtered[len(filtered)-1].ID
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"object": "list", "data": data, "first_id": firstID, "last_id": lastID, "has_more": hasMore,
	})
}

func (server *Server) retrieveFile(response http.ResponseWriter, request *http.Request) {
	if record := server.ownedFile(response, request); record != nil {
		writeJSON(response, http.StatusOK, openAIFile(*record))
	}
}

func (server *Server) retrieveContent(response http.ResponseWriter, request *http.Request) {
	record := server.ownedFile(response, request)
	if record == nil {
		return
	}
	response.Header().Set("Content-Type", record.MediaType)
	response.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", record.Filename))
	http.ServeFile(response, request, filepath.Join(server.settings.DataDir, record.SourcePath))
}

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

func openAIFile(record store.File) map[string]any {
	status := "uploaded"
	if record.Status == "processed" {
		status = "processed"
	} else if record.Status == "failed" {
		status = "error"
	}
	return map[string]any{
		"id": record.ID, "object": "file", "bytes": record.Bytes,
		"created_at": record.CreatedAt, "expires_at": record.ExpiresAt,
		"filename": record.Filename, "purpose": record.Purpose, "status": status,
	}
}

func writeAnyError(response http.ResponseWriter, request *http.Request, err error) {
	var gatewayError *apierror.Error
	if errors.As(err, &gatewayError) {
		writeError(response, gatewayError)
		return
	}
	slog.Error("request failed", "method", request.Method, "path", request.URL.Path, "error", err)
	writeError(response, apierror.New(500, "internal_error", "Internal server error.", ""))
}

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

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		slog.Debug("response write interrupted", "status", status, "error", err)
	}
}
