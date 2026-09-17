package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/config"
	"github.com/mochizuki875/llm-file-gateway/internal/converter"
	"github.com/mochizuki875/llm-file-gateway/internal/files"
	"github.com/mochizuki875/llm-file-gateway/internal/store"
)

func TestHealth(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	response := httptest.NewRecorder()

	NewHandler(settings, dataStore, service).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got, want := response.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestFileLifecycle(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	handler := NewHandler(settings, dataStore, service)
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	file, err := form.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, "verification code 4821")
	_ = form.WriteField("purpose", "user_data")
	_ = form.WriteField("expires_after", `{"anchor":"created_at","seconds":300}`)
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("Authorization", "Bearer client-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)

	deadline := time.Now().Add(5 * time.Second)
	for {
		request = httptest.NewRequest(http.MethodGet, "/v1/files/"+id, nil)
		request.Header.Set("Authorization", "Bearer client-key")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var current map[string]any
		_ = json.Unmarshal(response.Body.Bytes(), &current)
		if current["status"] == "processed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("file did not process: %s", response.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	request = httptest.NewRequest(http.MethodDelete, "/v1/files/"+id, nil)
	request.Header.Set("Authorization", "Bearer client-key")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.Code, response.Body.String())
	}
}

func TestMissingFileLogsWarning(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	handler := NewHandler(settings, dataStore, service)
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/v1/files/file_missing", nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want %d", method, response.Code, http.StatusNotFound)
		}
	}

	logOutput := output.String()
	if count := strings.Count(logOutput, `msg="file not found"`); count != 2 {
		t.Fatalf("file-not-found warnings = %d, want 2: %q", count, logOutput)
	}
	for _, expected := range []string{"level=WARN", "file_id=file_missing", "param=file_id"} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf("log output = %q, missing %q", logOutput, expected)
		}
	}
}

func TestInternalRequestErrorLogs(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	handler := NewHandler(settings, dataStore, service)
	if err := dataStore.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/files", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	logOutput := output.String()
	for _, expected := range []string{"level=ERROR", `msg="request failed"`, "method=GET", "path=/v1/files", `error="list files:`} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf("log output = %q, missing %q", logOutput, expected)
		}
	}
}

func TestResponsesExpandsInlineUnknownTextFormat(t *testing.T) {
	var upstreamPayload map[string]any
	var receivedAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		receivedAuthorization = request.Header.Get("Authorization")
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	baseURL, _ := url.Parse(upstream.URL + "/v1")
	settings.VLLMBaseURL = baseURL
	settings.VLLMModel = "test-model"
	settings.MaxDocumentImages = 8
	handler := NewHandler(settings, dataStore, service)
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_file", "filename": "data.json", "file_data": base64.StdEncoding.EncodeToString([]byte(`{"status":"ready"}`))},
				map[string]any{"type": "input_text", "text": "Summarize it."},
			},
		}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if receivedAuthorization != "" {
		t.Fatalf("upstream authorization = %q, want empty", receivedAuthorization)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("responses status = %d: %s", response.Code, response.Body.String())
	}
	items := upstreamPayload["input"].([]any)
	content := items[0].(map[string]any)["content"].([]any)
	first := content[0].(map[string]any)
	if first["type"] != "input_text" || !strings.Contains(first["text"].(string), `{"status":"ready"}`) {
		t.Fatalf("expanded content = %#v", content)
	}
	entries, err := os.ReadDir(filepath.Join(settings.DataDir, "work"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary entries = %v, %v", entries, err)
	}
}

func TestInferenceLogsUpstreamErrorResponse(t *testing.T) {
	upstreamBody := `{"error":{"message":"At most 8 image(s) may be provided in one prompt.","type":"BadRequestError","param":"image","code":400}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(response, upstreamBody)
	}))
	defer upstream.Close()

	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || response.Body.String() != upstreamBody {
		t.Fatalf("response = %d %q, want %d %q", response.Code, response.Body.String(), http.StatusBadRequest, upstreamBody)
	}
	logOutput := output.String()
	for _, expected := range []string{
		"level=ERROR", `msg="upstream inference returned error"`, "endpoint=responses", "status=400",
		"error_type=BadRequestError", "error_code=400", "error_param=image",
		`error_message="At most 8 image(s) may be provided in one prompt."`,
	} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf("log output = %q, missing %q", logOutput, expected)
		}
	}
}

func TestInferenceStreamsUpstreamEvents(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		payload  string
	}{
		{name: "responses", endpoint: "responses", payload: `{"model":"test-model","input":"hello","stream":true}`},
		{name: "chat_completions", endpoint: "chat/completions", payload: `{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			releaseSecondEvent := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(releaseSecondEvent) })

			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				if payload["stream"] != true {
					t.Errorf("stream = %#v, want true", payload["stream"])
				}
				response.Header().Set("Content-Type", "text/event-stream")
				response.Header().Set("Cache-Control", "no-cache")
				_, _ = io.WriteString(response, "data: first\n\n")
				response.(http.Flusher).Flush()
				<-releaseSecondEvent
				_, _ = io.WriteString(response, "data: second\n\n")
				response.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			settings, dataStore, service := testDependencies(t)
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			settings.VLLMModel = "test-model"
			gateway := httptest.NewServer(NewHandler(settings, dataStore, service))
			defer gateway.Close()

			response, err := http.Post(
				gateway.URL+"/v1/"+test.endpoint,
				"application/json",
				strings.NewReader(test.payload),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d: %s", response.StatusCode, body)
			}
			if response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-cache" {
				t.Fatalf("headers = %v", response.Header)
			}

			reader := bufio.NewReader(response.Body)
			firstEvent := make(chan string, 1)
			go func() {
				line, _ := reader.ReadString('\n')
				firstEvent <- line
			}()
			select {
			case line := <-firstEvent:
				if line != "data: first\n" {
					t.Fatalf("first event = %q", line)
				}
			case <-time.After(time.Second):
				releaseOnce.Do(func() { close(releaseSecondEvent) })
				t.Fatal("first event was not flushed before the upstream stream completed")
			}

			releaseOnce.Do(func() { close(releaseSecondEvent) })
			remainder, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(remainder), "data: second\n\n") {
				t.Fatalf("remaining events = %q", remainder)
			}
		})
	}
}

func TestResponsesReportsDocumentTextLimit(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.VLLMModel = "test-model"
	settings.MaxDocumentTextChars = 4
	handler := NewHandler(settings, dataStore, service)
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "filename": "notes.txt",
				"file_data": base64.StdEncoding.EncodeToString([]byte("12345")),
			}},
		}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error.Code != "document_text_too_large" {
		t.Fatalf("error code = %q", result.Error.Code)
	}
	entries, err := os.ReadDir(filepath.Join(settings.DataDir, "work"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary entries = %v, %v", entries, err)
	}
}

func TestDocumentPartsReportsMissingArtifact(t *testing.T) {
	settings, _, _ := testDependencies(t)
	server := &Server{settings: settings}
	document := resolvedDocument{
		filename:   "notes.txt",
		derivedDir: t.TempDir(),
		manifest: converter.Manifest{Documents: []converter.ManifestDocument{{Parts: []converter.Artifact{{
			PartNumber: 1,
			TextPath:   "missing.txt",
		}}}}},
	}

	if _, err := server.documentParts(document, "responses"); err == nil {
		t.Fatal("documentParts() succeeded with a missing text artifact")
	}
}

func TestDocumentPartsOmitsEmptyText(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.MaxDocumentImages = 1
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(derivedDir, "page.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(derivedDir, "page.png"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	imagePath := "page.png"
	document := resolvedDocument{
		filename:   "document.pdf",
		derivedDir: derivedDir,
		manifest: converter.Manifest{Documents: []converter.ManifestDocument{{Parts: []converter.Artifact{{
			PartNumber: 1, TextPath: "page.txt", ImagePath: &imagePath,
		}}}}},
	}

	parts, err := server.documentParts(document, "responses")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].(map[string]any)["type"] != "input_image" {
		t.Fatalf("parts = %#v, want one input_image", parts)
	}
}

func TestPassthroughPreservesRequestAndResponse(t *testing.T) {
	var receivedMethod, receivedQuery, receivedBody, receivedRequestID, receivedAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		receivedMethod = request.Method
		receivedQuery = request.URL.RawQuery
		body, _ := io.ReadAll(request.Body)
		receivedBody = string(body)
		receivedRequestID = request.Header.Get("X-Request-ID")
		receivedAuthorization = request.Header.Get("Authorization")
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("X-VLLM-Request-ID", "upstream-1")
		response.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(response, `{"object":"passthrough"}`)
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	handler := NewHandler(settings, dataStore, service)
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings?encoding_format=float&tag=one&tag=two", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Authorization", "Bearer client-key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "client-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || response.Header().Get("X-VLLM-Request-ID") != "upstream-1" {
		t.Fatalf("response = %d, headers=%v", response.Code, response.Header())
	}
	if receivedMethod != http.MethodPost || receivedQuery != "encoding_format=float&tag=one&tag=two" || receivedBody != `{"input":"hello"}` {
		t.Fatalf("request = method=%q query=%q body=%q", receivedMethod, receivedQuery, receivedBody)
	}
	if receivedRequestID != "client-1" || receivedAuthorization != "Bearer client-key" {
		t.Fatalf("headers = request-id=%q authorization=%q", receivedRequestID, receivedAuthorization)
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, httptest.NewRequest(http.MethodPatch, "/v1/files/file_unknown", strings.NewReader(`{}`)))
	if blocked.Code != http.StatusMethodNotAllowed {
		t.Fatalf("files PATCH status = %d", blocked.Code)
	}
}

func TestSafeHTTPErrorOmitsURL(t *testing.T) {
	underlying := errors.New("connection refused")
	err := &url.Error{Op: "Get", URL: "https://example.com/file?token=secret", Err: underlying}
	if got := safeHTTPError(err); !errors.Is(got, underlying) || strings.Contains(got.Error(), "secret") {
		t.Fatalf("safeHTTPError() = %q, want underlying error without URL", got)
	}
}

func TestRequiredAuthenticationUsesUpstreamKey(t *testing.T) {
	var receivedAuthorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		receivedAuthorizations = append(receivedAuthorizations, request.Header.Get("Authorization"))
		writeJSON(response, http.StatusOK, map[string]any{"data": []any{}})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.GatewayAuthRequired = true
	settings.GatewayAPIKey = "gateway-key"
	settings.VLLMAPIKey = "upstream-key"
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d", unauthorized.Code)
	}
	invalidRequest := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	invalidRequest.Header.Set("Authorization", "Bearer wrong-key")
	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, invalidRequest)
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid auth status = %d", invalid.Code)
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	payload := strings.NewReader(`{"model":"test-model","input":"hello"}`)
	inferenceRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", payload)
	inferenceRequest.Header.Set("Authorization", "Bearer gateway-key")
	inference := httptest.NewRecorder()
	handler.ServeHTTP(inference, inferenceRequest)
	if inference.Code != http.StatusOK {
		t.Fatalf("inference response = %d: %s", inference.Code, inference.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer gateway-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d", response.Code)
	}
	if len(receivedAuthorizations) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(receivedAuthorizations))
	}
	for _, authorization := range receivedAuthorizations {
		if authorization != "Bearer upstream-key" {
			t.Fatalf("upstream authorization = %q", authorization)
		}
	}
}

func testDependencies(t *testing.T) (config.Config, *store.Store, *files.Service) {
	t.Helper()
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	settings := config.Config{
		DataDir: dataDir, FileTTL: 5 * time.Minute, MaxFileBytes: 1024,
		MaxDocumentPages: 20, MaxDocumentTextChars: 500_000, ConversionWorkers: 2,
	}
	if err := os.Mkdir(filepath.Join(dataDir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := files.New(settings, dataStore)
	ctx, cancel := context.WithCancel(context.Background())
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		service.Stop()
		_ = dataStore.Close()
	})
	return settings, dataStore, service
}
