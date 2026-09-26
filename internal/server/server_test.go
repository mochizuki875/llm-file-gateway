package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
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

type blockingHeaderWriter struct {
	*httptest.ResponseRecorder
	reached chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (writer *blockingHeaderWriter) Header() http.Header {
	writer.once.Do(func() {
		close(writer.reached)
		<-writer.proceed
	})
	return writer.ResponseRecorder.Header()
}

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

func TestRequestDirectoryUsesOwnerOnlyPermissions(t *testing.T) {
	directory, err := createRequestDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("request directory mode = %04o, want 0700", got)
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
	_ = form.WriteField("purpose", "assistants")
	_ = form.WriteField("expires_after", `{"anchor":"created_at","seconds":120}`)
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
	if got, want := created["purpose"], "assistants"; got != want {
		t.Fatalf("purpose = %q, want %q", got, want)
	}
	id := created["id"].(string)
	createdAt := int64(created["created_at"].(float64))
	expiresAt := int64(created["expires_at"].(float64))
	if expiresAt-createdAt != 120 {
		t.Fatalf("file lifetime = %d seconds, want 120", expiresAt-createdAt)
	}

	invalidBody := &bytes.Buffer{}
	invalidForm := multipart.NewWriter(invalidBody)
	invalidFile, err := invalidForm.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(invalidFile, "invalid purpose")
	_ = invalidForm.WriteField("purpose", "unsupported")
	if err := invalidForm.Close(); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/files", invalidBody)
	request.Header.Set("Content-Type", invalidForm.FormDataContentType())
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid purpose status = %d: %s", response.Code, response.Body.String())
	}

	// Output purposes are not valid Files Create API inputs.
	for _, purpose := range []string{"assistants_output", "batch_output", "fine-tune-results"} {
		outputBody := &bytes.Buffer{}
		outputForm := multipart.NewWriter(outputBody)
		outputFile, err := outputForm.CreateFormFile("file", "notes.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(outputFile, "output purpose")
		_ = outputForm.WriteField("purpose", purpose)
		if err := outputForm.Close(); err != nil {
			t.Fatal(err)
		}
		request = httptest.NewRequest(http.MethodPost, "/v1/files", outputBody)
		request.Header.Set("Content-Type", outputForm.FormDataContentType())
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("purpose %q status = %d: %s", purpose, response.Code, response.Body.String())
		}
	}

	// evals is a valid Files Create API purpose.
	evalsBody := &bytes.Buffer{}
	evalsForm := multipart.NewWriter(evalsBody)
	evalsFile, err := evalsForm.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(evalsFile, "evals purpose")
	_ = evalsForm.WriteField("purpose", "evals")
	if err := evalsForm.Close(); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/files", evalsBody)
	request.Header.Set("Content-Type", evalsForm.FormDataContentType())
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("evals purpose status = %d: %s", response.Code, response.Body.String())
	}

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
	record, err := dataStore.GetInternal(context.Background(), id)
	if err != nil || record == nil {
		t.Fatalf("stored file = %#v, %v", record, err)
	}
	fileDirectory := filepath.Dir(filepath.Join(settings.DataDir, record.SourcePath))

	request = httptest.NewRequest(http.MethodDelete, "/v1/files/"+id, nil)
	request.Header.Set("Authorization", "Bearer client-key")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.Code, response.Body.String())
	}
	if record, err := dataStore.GetInternal(context.Background(), id); err != nil || record != nil {
		t.Fatalf("deleted database record = %#v, %v; want nil, nil", record, err)
	}
	if _, err := os.Stat(fileDirectory); !os.IsNotExist(err) {
		t.Fatalf("deleted file directory stat error = %v; want not exist", err)
	}
}

func TestCreateFileExpiration(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	handler := NewHandler(settings, dataStore, service)

	for _, test := range []struct {
		name         string
		expiresAfter map[string]string
		wantStatus   int
		wantLifetime int64
	}{
		{name: "default", wantStatus: http.StatusOK, wantLifetime: int64(settings.FileTTL / time.Second)},
		{name: "json_custom", expiresAfter: map[string]string{"expires_after": `{"anchor":"created_at","seconds":45}`}, wantStatus: http.StatusOK, wantLifetime: 45},
		{name: "sdk_custom", expiresAfter: map[string]string{"expires_after[anchor]": "created_at", "expires_after[seconds]": "45"}, wantStatus: http.StatusOK, wantLifetime: 45},
		{name: "maximum", expiresAfter: map[string]string{"expires_after[anchor]": "created_at", "expires_after[seconds]": "300"}, wantStatus: http.StatusOK, wantLifetime: 300},
		{name: "over_maximum", expiresAfter: map[string]string{"expires_after[anchor]": "created_at", "expires_after[seconds]": "301"}, wantStatus: http.StatusBadRequest},
		{name: "invalid_anchor", expiresAfter: map[string]string{"expires_after[anchor]": "uploaded_at", "expires_after[seconds]": "45"}, wantStatus: http.StatusBadRequest},
		{name: "non_positive", expiresAfter: map[string]string{"expires_after[anchor]": "created_at", "expires_after[seconds]": "0"}, wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &bytes.Buffer{}
			form := multipart.NewWriter(body)
			file, err := form.CreateFormFile("file", "notes.txt")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(file, "expiration test")
			_ = form.WriteField("purpose", "user_data")
			for name, value := range test.expiresAfter {
				_ = form.WriteField(name, value)
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
			request.Header.Set("Content-Type", form.FormDataContentType())
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantStatus != http.StatusOK {
				return
			}
			var created map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			lifetime := int64(created["expires_at"].(float64) - created["created_at"].(float64))
			if lifetime != test.wantLifetime {
				t.Fatalf("file lifetime = %d seconds, want %d", lifetime, test.wantLifetime)
			}
		})
	}
}

func TestCreateFileUnlimitedTTL(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.FileTTL = 0
	handler := NewHandler(settings, dataStore, service)

	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	file, err := form.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, "never expires")
	_ = form.WriteField("purpose", "user_data")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["expires_at"] != nil {
		t.Fatalf("expires_at = %#v, want null (never expires)", created["expires_at"])
	}

	// With an unlimited TTL, expires_after accepts any positive seconds.
	body = &bytes.Buffer{}
	form = multipart.NewWriter(body)
	file, err = form.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, "custom expiry")
	_ = form.WriteField("purpose", "user_data")
	_ = form.WriteField("expires_after[anchor]", "created_at")
	_ = form.WriteField("expires_after[seconds]", "999999")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/files", body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	lifetime := int64(created["expires_at"].(float64) - created["created_at"].(float64))
	if lifetime != 999999 {
		t.Fatalf("file lifetime = %d seconds, want 999999", lifetime)
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

func TestValidationErrorLogsWarning(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	handler := NewHandler(settings, dataStore, service)
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/files?limit=0", nil))

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
	logOutput := output.String()
	for _, expected := range []string{"level=WARN", `msg="request rejected"`, "status=422", "code=invalid_request", "param=limit"} {
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
	settings.MaxDocumentPages = 8
	handler := NewHandler(settings, dataStore, service)
	payload := map[string]any{
		"model":                "test-model",
		"previous_response_id": "resp_previous",
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
	if receivedAuthorization != "Bearer upstream-key" {
		t.Fatalf("upstream authorization = %q, want VLLM API key", receivedAuthorization)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("responses status = %d: %s", response.Code, response.Body.String())
	}
	if upstreamPayload["previous_response_id"] != "resp_previous" {
		t.Fatalf("previous_response_id = %#v, want resp_previous", upstreamPayload["previous_response_id"])
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

func TestInferenceForwardsSafeRequestHeaders(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		payload  map[string]any
	}{
		{
			name: "responses", endpoint: "/v1/responses",
			payload: map[string]any{"model": "test-model", "input": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{
					"type": "input_file", "filename": "notes.txt",
					"file_data": base64.StdEncoding.EncodeToString([]byte("secret")),
				}},
			}}},
		},
		{
			name: "chat", endpoint: "/v1/chat/completions",
			payload: map[string]any{"model": "test-model", "messages": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{
					"type": "file", "file": map[string]any{
						"filename": "notes.txt", "file_data": base64.StdEncoding.EncodeToString([]byte("secret")),
					},
				}},
			}}},
		},
		{name: "responses_without_file", endpoint: "/v1/responses", payload: map[string]any{"model": "test-model", "input": "hello"}},
		{name: "chat_without_file", endpoint: "/v1/chat/completions", payload: map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var receivedHeader http.Header
			var receivedHost string
			var receivedLength int64
			var receivedBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				receivedHeader = request.Header.Clone()
				receivedHost = request.Host
				receivedLength = request.ContentLength
				receivedBody, _ = io.ReadAll(request.Body)
				writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
			}))
			defer upstream.Close()

			settings, dataStore, service := testDependencies(t)
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			settings.VLLMModel = "test-model"
			handler := NewHandler(settings, dataStore, service)
			body, _ := json.Marshal(test.payload)
			request := httptest.NewRequest(http.MethodPost, test.endpoint, bytes.NewReader(body))
			request.Host = "client.example"
			request.ContentLength = 1
			request.Header.Set("Content-Length", "1")
			request.Header.Set("Content-Type", "text/plain")
			request.Header.Set("Authorization", "Bearer client-key")
			request.Header.Set("X-Request-ID", "request-123")
			request.Header.Set("X-Correlation-ID", "correlation-456")
			request.Header.Set("Traceparent", "00-trace-parent")
			request.Header.Set("Tracestate", "vendor=value")
			request.Header.Set("Baggage", "key=value")
			request.Header.Set("X-Custom-Metadata", "custom")
			request.Header.Set("Connection", "X-Custom-Hop")
			request.Header.Set("X-Custom-Hop", "remove")
			request.Header.Set("Proxy-Connection", "keep-alive")
			request.Header.Set("Keep-Alive", "timeout=5")
			request.Header.Set("TE", "trailers")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			for name, want := range map[string]string{
				"X-Request-ID": "request-123", "X-Correlation-ID": "correlation-456",
				"Traceparent": "00-trace-parent", "Tracestate": "vendor=value",
				"Baggage": "key=value", "X-Custom-Metadata": "custom",
			} {
				if got := receivedHeader.Get(name); got != want {
					t.Errorf("upstream %s = %q, want %q", name, got, want)
				}
			}
			if got := receivedHeader.Get("Authorization"); got != "Bearer upstream-key" {
				t.Errorf("upstream Authorization = %q", got)
			}
			for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "TE", "X-Custom-Hop"} {
				if got := receivedHeader.Get(name); got != "" {
					t.Errorf("upstream %s = %q, want empty", name, got)
				}
			}
			upstreamURL, _ := url.Parse(upstream.URL)
			if receivedHost != upstreamURL.Host {
				t.Errorf("upstream Host = %q, want %q", receivedHost, upstreamURL.Host)
			}
			if got := receivedHeader.Get("Content-Type"); got != "application/json" {
				t.Errorf("upstream Content-Type = %q", got)
			}
			if receivedLength != int64(len(receivedBody)) || receivedLength == 1 {
				t.Errorf("upstream Content-Length = %d, body length = %d", receivedLength, len(receivedBody))
			}
		})
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
			defer func() { _ = response.Body.Close() }()
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

func TestInlineFileDataExactLimitAccepted(t *testing.T) {
	var upstreamPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	settings.MaxFileBytes = 4 << 20
	settings.MaxDocumentTextChars = 10 << 20
	handler := NewHandler(settings, dataStore, service)

	content := strings.Repeat("a", int(settings.MaxFileBytes))
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "filename": "notes.txt",
				"file_data": base64.StdEncoding.EncodeToString([]byte(content)),
			}},
		}},
	}
	body, _ := json.Marshal(payload)
	// The encoded body must exceed the old raw-file limit (MaxFileBytes + 1 MiB)
	// so this test reproduces the bug where base64 expansion alone caused a
	// rejection before the decoded size was checked.
	if len(body) <= int(settings.MaxFileBytes)+1<<20 {
		t.Fatalf("test setup: encoded body = %d bytes, want > %d", len(body), int(settings.MaxFileBytes)+1<<20)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}

func TestInlineFileDataOverDecodedLimitRejected(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.VLLMModel = "test-model"
	settings.MaxFileBytes = 4 << 20
	settings.MaxDocumentTextChars = 10 << 20
	handler := NewHandler(settings, dataStore, service)

	content := strings.Repeat("a", int(settings.MaxFileBytes)+1)
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "filename": "notes.txt",
				"file_data": base64.StdEncoding.EncodeToString([]byte(content)),
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
	if result.Error.Code != "file_too_large" {
		t.Fatalf("error code = %q, want file_too_large", result.Error.Code)
	}
}

func TestInlineFileDataOversizedBodyRejected(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.VLLMModel = "test-model"
	settings.MaxFileBytes = 4 << 20
	settings.MaxRequestBodyBytes = int64((settings.MaxFileBytes+2)/3)*4 + 1<<20
	handler := NewHandler(settings, dataStore, service)

	content := strings.Repeat("a", int(settings.MaxFileBytes)*2)
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "filename": "notes.txt",
				"file_data": base64.StdEncoding.EncodeToString([]byte(content)),
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
	if result.Error.Code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", result.Error.Code)
	}
}

func TestInlineFileDataUnlimitedBodyAccepted(t *testing.T) {
	var upstreamPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	// Both limits unlimited: the request body must not be capped.
	settings.MaxFileBytes = 0
	settings.MaxRequestBodyBytes = 0
	settings.MaxDocumentTextChars = 10 << 20
	handler := NewHandler(settings, dataStore, service)

	content := strings.Repeat("a", 1<<20)
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "filename": "notes.txt",
				"file_data": base64.StdEncoding.EncodeToString([]byte(content)),
			}},
		}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}

func TestUnlimitedRequestBodyKeepsPerFileLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	settings.MaxFileBytes = 16
	settings.MaxRequestBodyBytes = 0
	handler := NewHandler(settings, dataStore, service)

	largeBody, _ := json.Marshal(map[string]any{
		"model": "test-model", "input": "hello",
		"metadata": map[string]any{"padding": strings.Repeat("x", 2<<20)},
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(largeBody))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unlimited body status = %d: %s", response.Code, response.Body.String())
	}

	fileBody, _ := json.Marshal(map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "input_file", "filename": "notes.txt",
			"file_data": base64.StdEncoding.EncodeToString([]byte("seventeen bytes!!")),
		}}}},
	})
	request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(fileBody))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "file_too_large") {
		t.Fatalf("per-file limit response = %d: %s", response.Code, response.Body.String())
	}
}

func TestInferencePreservesLargeJSONIntegers(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		body     string
	}{
		{name: "responses", endpoint: "/v1/responses", body: `{"model":"test-model","input":"hello","seed":9007199254740993,"metadata":{"values":[9007199254740995]}}`},
		{name: "chat", endpoint: "/v1/chat/completions", body: `{"model":"test-model","messages":[{"role":"user","content":"hello"}],"seed":9007199254740993,"metadata":{"values":[9007199254740995]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var received string
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				content, _ := io.ReadAll(request.Body)
				received = string(content)
				writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
			}))
			defer upstream.Close()
			settings, dataStore, service := testDependencies(t)
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			settings.VLLMModel = "test-model"
			handler := NewHandler(settings, dataStore, service)

			request := httptest.NewRequest(http.MethodPost, test.endpoint, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			for _, number := range []string{"9007199254740993", "9007199254740995"} {
				if !strings.Contains(received, number) {
					t.Fatalf("upstream payload = %s, missing %s", received, number)
				}
			}
		})
	}
}

func TestInferenceRequiresSingleJSONDocument(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "trailing_whitespace", body: "{\"model\":\"test-model\",\"input\":\"hello\"}\n\t", wantStatus: http.StatusOK},
		{name: "second_object", body: `{"model":"test-model","input":"hello"} {}`, wantStatus: http.StatusBadRequest},
		{name: "second_number", body: `{"model":"test-model","input":"hello"} 123`, wantStatus: http.StatusBadRequest},
		{name: "trailing_garbage", body: `{"model":"test-model","input":"hello"} garbage`, wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
			}))
			defer upstream.Close()
			settings, dataStore, service := testDependencies(t)
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			settings.VLLMModel = "test-model"
			handler := NewHandler(settings, dataStore, service)

			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d: %s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
		})
	}
}

func TestInlineFileDataMalformedBase64Rejected(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "filename": "notes.txt",
				"file_data": "not valid base64!!!",
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
	if result.Error.Code != "invalid_file_data" {
		t.Fatalf("error code = %q, want invalid_file_data", result.Error.Code)
	}
}

func TestInlineFileDataMultiFileTotalLimit(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.VLLMModel = "test-model"
	settings.MaxFileBytes = 4 << 20
	settings.MaxRequestBodyBytes = int64((settings.MaxFileBytes+2)/3)*4 + 1<<20
	handler := NewHandler(settings, dataStore, service)

	content := strings.Repeat("a", int(settings.MaxFileBytes))
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_file", "filename": "a.txt", "file_data": base64.StdEncoding.EncodeToString([]byte(content))},
				map[string]any{"type": "input_file", "filename": "b.txt", "file_data": base64.StdEncoding.EncodeToString([]byte(content))},
			},
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
	if result.Error.Code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", result.Error.Code)
	}
}

// TestInlineFileDataAggregateLimit verifies that MAX_REQUEST_BODY_BYTES is
// enforced as an aggregate limit: multiple inline files that are each within
// MAX_FILE_BYTES are rejected when their combined base64-encoded body exceeds
// the configured aggregate limit.
func TestInlineFileDataAggregateLimit(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.VLLMModel = "test-model"
	settings.MaxFileBytes = 4 << 20
	// Aggregate limit allows one file plus overhead but not two.
	settings.MaxRequestBodyBytes = int64((4<<20+2)/3)*4 + 1<<20
	handler := NewHandler(settings, dataStore, service)

	content := strings.Repeat("a", int(settings.MaxFileBytes))
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_file", "filename": "a.txt", "file_data": base64.StdEncoding.EncodeToString([]byte(content))},
				map[string]any{"type": "input_file", "filename": "b.txt", "file_data": base64.StdEncoding.EncodeToString([]byte(content))},
			},
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
	if result.Error.Code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", result.Error.Code)
	}
}

// TestInlineFileDataWithinAggregateLimitAccepted verifies that a single file
// within MAX_FILE_BYTES is accepted when MAX_REQUEST_BODY_BYTES is set to a
// value that accommodates its base64 expansion.
func TestInlineFileDataWithinAggregateLimitAccepted(t *testing.T) {
	var upstreamPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	settings.MaxFileBytes = 4 << 20
	settings.MaxDocumentTextChars = 10 << 20
	settings.MaxRequestBodyBytes = int64((4<<20+2)/3)*4 + 1<<20
	handler := NewHandler(settings, dataStore, service)

	content := strings.Repeat("a", int(settings.MaxFileBytes))
	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "filename": "notes.txt",
				"file_data": base64.StdEncoding.EncodeToString([]byte(content)),
			}},
		}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
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

func TestDocumentPartsEmitsDocumentTextBeforeImages(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.MaxDocumentPages = 1
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(derivedDir, "document.txt"), []byte("complete document text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(derivedDir, "page.png"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	imagePath := "page.png"
	pageNumber := 1
	document := resolvedDocument{
		filename:   "document.docx",
		derivedDir: derivedDir,
		manifest: converter.Manifest{Documents: []converter.ManifestDocument{{
			TextPath: "document.txt",
			Parts: []converter.Artifact{{
				PartNumber: 1, PageNumber: &pageNumber, ImagePath: &imagePath,
			}},
		}}},
	}

	parts, err := server.documentParts(document, "responses")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %#v, want document text and one image", parts)
	}
	text := parts[0].(map[string]any)
	if text["type"] != "input_text" || !strings.Contains(text["text"].(string), "complete document text") || strings.Contains(text["text"].(string), "page=") {
		t.Fatalf("document text part = %#v", text)
	}
	if parts[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("image part = %#v", parts[1])
	}
}

func TestDocumentPartsOmitsEmptyText(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.MaxDocumentPages = 1
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

func TestDocumentPartsUnlimitedPagesEmitsAllImages(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.MaxDocumentPages = 0
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	imagePath := "page.png"
	if err := os.WriteFile(filepath.Join(derivedDir, imagePath), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	document := resolvedDocument{
		filename:   "document.pdf",
		derivedDir: derivedDir,
		manifest: converter.Manifest{Documents: []converter.ManifestDocument{{Parts: []converter.Artifact{
			{PartNumber: 1, ImagePath: &imagePath},
			{PartNumber: 2, ImagePath: &imagePath},
			{PartNumber: 3, ImagePath: &imagePath},
		}}}},
	}

	parts, err := server.documentParts(document, "responses")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3 (unlimited)", len(parts))
	}
	for _, part := range parts {
		if part.(map[string]any)["type"] != "input_image" {
			t.Fatalf("part = %#v, want input_image", part)
		}
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
	if receivedRequestID != "client-1" || receivedAuthorization != "Bearer upstream-key" {
		t.Fatalf("headers = request-id=%q authorization=%q", receivedRequestID, receivedAuthorization)
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, httptest.NewRequest(http.MethodPatch, "/v1/files/file_unknown", strings.NewReader(`{}`)))
	if blocked.Code != http.StatusMethodNotAllowed {
		t.Fatalf("files PATCH status = %d", blocked.Code)
	}
}

func TestPassthroughRemovesConnectionListedHeaders(t *testing.T) {
	var receivedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		receivedHeaders = request.Header.Clone()
		writeJSON(response, http.StatusOK, map[string]any{"object": "passthrough"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	handler := NewHandler(settings, dataStore, service)
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Connection", "X-Test-Hop, keep-alive")
	request.Header.Set("X-Test-Hop", "secret-value")
	request.Header.Set("X-Keep", "forwarded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if receivedHeaders.Get("X-Test-Hop") != "" {
		t.Fatalf("upstream received Connection-listed header X-Test-Hop = %q", receivedHeaders.Get("X-Test-Hop"))
	}
	if receivedHeaders.Get("Connection") != "" {
		t.Fatalf("upstream received Connection header = %q", receivedHeaders.Get("Connection"))
	}
	if receivedHeaders.Get("X-Keep") != "forwarded" {
		t.Fatalf("upstream X-Keep = %q, want forwarded", receivedHeaders.Get("X-Keep"))
	}
}

// TestPassthroughRemovesProxyConnectionHeader verifies that the hop-by-hop
// Proxy-Connection header is stripped before the request is forwarded to the
// upstream server.
func TestPassthroughRemovesProxyConnectionHeader(t *testing.T) {
	var receivedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		receivedHeaders = request.Header.Clone()
		writeJSON(response, http.StatusOK, map[string]any{"object": "passthrough"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	handler := NewHandler(settings, dataStore, service)
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Proxy-Connection", "keep-alive")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if receivedHeaders.Get("Proxy-Connection") != "" {
		t.Fatalf("upstream received Proxy-Connection header = %q", receivedHeaders.Get("Proxy-Connection"))
	}
}

func TestSafeHTTPErrorOmitsURL(t *testing.T) {
	underlying := errors.New("connection refused")
	err := &url.Error{Op: "Get", URL: "https://example.com/file?token=secret", Err: underlying}
	if got := safeHTTPError(err); !errors.Is(got, underlying) || strings.Contains(got.Error(), "secret") {
		t.Fatalf("safeHTTPError() = %q, want underlying error without URL", got)
	}
}

func TestUpstreamTimeoutReturnsModelTimeout(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		payload  string
	}{
		{name: "responses", endpoint: "/v1/responses", payload: `{"model":"test-model","input":"hello"}`},
		{name: "chat_completions", endpoint: "/v1/chat/completions", payload: `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`},
		{name: "passthrough", endpoint: "/v1/embeddings", payload: `{"input":"hello"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A server that never responds causes the client to time out.
			// The handler sleeps longer than the client timeout so that the
			// request always times out, but still returns so that the test
			// server can be closed.
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				time.Sleep(500 * time.Millisecond)
			}))
			defer upstream.Close()
			settings, dataStore, service := testDependencies(t)
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			settings.VLLMModel = "test-model"
			settings.RequestTimeout = 50 * time.Millisecond
			handler := NewHandler(settings, dataStore, service)

			request := httptest.NewRequest(http.MethodPost, test.endpoint, strings.NewReader(test.payload))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusGatewayTimeout {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusGatewayTimeout, response.Body.String())
			}
			var result struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error.Code != "model_timeout" {
				t.Fatalf("error code = %q, want model_timeout", result.Error.Code)
			}
		})
	}
}

func TestUpstreamUnreachableReturnsModelUpstreamError(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse("http://127.0.0.1:1/v1")
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadGateway, response.Body.String())
	}
	var result struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error.Code != "model_upstream_error" {
		t.Fatalf("error code = %q, want model_upstream_error", result.Error.Code)
	}
}

func TestDisabledAuthenticationUsesSharedTenant(t *testing.T) {
	server := &Server{}
	requests := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/files", nil),
		httptest.NewRequest(http.MethodGet, "/v1/files", nil),
	}
	requests[1].Header.Set("Authorization", "Bearer unverified-client-key")
	for _, request := range requests {
		tenantID, gatewayError := server.tenantID(request)
		if gatewayError != nil || tenantID != store.SharedTenantID {
			t.Fatalf("tenant = %q, error = %v; want %q, nil", tenantID, gatewayError, store.SharedTenantID)
		}
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

func TestRetrieveContent(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	handler := NewHandler(settings, dataStore, service)
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	file, err := form.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, "content payload 123")
	_ = form.WriteField("purpose", "user_data")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	request.Header.Set("Content-Type", form.FormDataContentType())
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

	request = httptest.NewRequest(http.MethodGet, "/v1/files/"+id+"/content", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("content status = %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("content type = %q, want text/plain", got)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, "notes.txt") {
		t.Fatalf("content disposition = %q", got)
	}
	if got := response.Body.String(); got != "content payload 123" {
		t.Fatalf("content = %q", got)
	}
}

// TestRetrieveContentSurvivesConcurrentDelete verifies the HTTP handler keeps
// the metadata snapshot acquired with its content lease after DELETE marks the
// record as deleted.
func TestRetrieveContentSurvivesConcurrentDelete(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	handler := NewHandler(settings, dataStore, service)
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	file, err := form.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, "content payload 123")
	_ = form.WriteField("purpose", "user_data")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	request.Header.Set("Content-Type", form.FormDataContentType())
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

	writer := &blockingHeaderWriter{
		ResponseRecorder: httptest.NewRecorder(),
		reached:          make(chan struct{}),
		proceed:          make(chan struct{}),
	}
	downloadDone := make(chan struct{})
	go func() {
		defer close(downloadDone)
		request := httptest.NewRequest(http.MethodGet, "/v1/files/"+id+"/content", nil)
		handler.ServeHTTP(writer, request)
	}()
	<-writer.reached

	deleted := make(chan bool, 1)
	go func() {
		ok, err := service.Delete(context.Background(), id, store.SharedTenantID)
		if err != nil {
			t.Errorf("Delete() error = %v", err)
			deleted <- false
			return
		}
		deleted <- ok
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		record, err := dataStore.GetInternal(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if record != nil && record.DeletedAt.Valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("DELETE did not mark the record")
		}
	}
	close(writer.proceed)
	<-downloadDone
	if writer.Code != http.StatusOK || writer.Body.String() != "content payload 123" {
		t.Fatalf("download response = %d %q", writer.Code, writer.Body.String())
	}

	select {
	case ok := <-deleted:
		if !ok {
			t.Fatal("Delete() reported no file deleted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete() did not complete after content release")
	}
}

func TestListFiles(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	handler := NewHandler(settings, dataStore, service)
	create := func(name, purpose string) string {
		t.Helper()
		body := &bytes.Buffer{}
		form := multipart.NewWriter(body)
		file, err := form.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(file, "list test content")
		_ = form.WriteField("purpose", purpose)
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
		request.Header.Set("Content-Type", form.FormDataContentType())
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("create %s status = %d: %s", name, response.Code, response.Body.String())
		}
		var created map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		return created["id"].(string)
	}
	firstID := create("first.txt", "user_data")
	// created_at is stored with second precision, so wait between creates to
	// make the cursor comparison in the after-filter deterministic.
	time.Sleep(1100 * time.Millisecond)
	secondID := create("second.txt", "assistants")

	request := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", response.Code, response.Body.String())
	}
	var list struct {
		Object  string           `json:"object"`
		Data    []map[string]any `json:"data"`
		FirstID any              `json:"first_id"`
		LastID  any              `json:"last_id"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 2 || list.HasMore {
		t.Fatalf("list = %#v", list)
	}
	// Default order is desc (newest first), so the second file is first.
	if list.FirstID != secondID || list.LastID != firstID {
		t.Fatalf("first_id = %v, last_id = %v", list.FirstID, list.LastID)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/files?limit=1", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || !list.HasMore {
		t.Fatalf("limited list = %#v", list)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/files?purpose=assistants", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0]["id"] != secondID {
		t.Fatalf("purpose-filtered list = %#v", list)
	}

	// created_at is stored with second precision, so wait before creating
	// third.txt to keep the after-filter below deterministic.
	time.Sleep(1100 * time.Millisecond)
	_ = create("third.txt", "user_data")
	time.Sleep(1100 * time.Millisecond)
	fourthID := create("fourth.txt", "assistants")
	request = httptest.NewRequest(http.MethodGet, "/v1/files?purpose=assistants&limit=1", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0]["id"] != fourthID || !list.HasMore {
		t.Fatalf("first purpose-filtered page = %#v", list)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/files?purpose=assistants&limit=1&after="+fourthID, nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0]["id"] != secondID || list.HasMore {
		t.Fatalf("second purpose-filtered page = %#v", list)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/files?after="+secondID, nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0]["id"] != firstID {
		t.Fatalf("after-filtered list = %#v", list)
	}

	for _, query := range []string{"limit=0", "limit=10001", "limit=abc", "order=sideways"} {
		request = httptest.NewRequest(http.MethodGet, "/v1/files?"+query, nil)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("query %q status = %d, want 422", query, response.Code)
		}
	}
}

func TestExpandChat(t *testing.T) {
	var upstreamPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "chat_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)

	payload := map[string]any{
		"model": "test-model",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "file", "file": map[string]any{
					"file_data": base64.StdEncoding.EncodeToString([]byte("chat document")),
					"filename":  "notes.txt",
				}},
				map[string]any{"type": "text", "text": "Summarize."},
			},
		}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("chat status = %d: %s", response.Code, response.Body.String())
	}
	messages := upstreamPayload["messages"].([]any)
	system := messages[0].(map[string]any)
	if system["role"] != "system" || !strings.Contains(system["content"].(string), "untrusted content") {
		t.Fatalf("system message = %#v", system)
	}
	content := messages[1].(map[string]any)["content"].([]any)
	first := content[0].(map[string]any)
	if first["type"] != "text" || !strings.Contains(first["text"].(string), "chat document") {
		t.Fatalf("expanded content = %#v", content)
	}
	entries, err := os.ReadDir(filepath.Join(settings.DataDir, "work"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary entries = %v, %v", entries, err)
	}
}

func TestExpandChatRejectsFileURL(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)
	payload := map[string]any{
		"model": "test-model",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "file", "file": map[string]any{"file_url": "https://example.com/notes.txt"},
			}},
		}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
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
	if result.Error.Code != "invalid_file_reference" {
		t.Fatalf("error code = %q", result.Error.Code)
	}
}

func TestExpandChatRejectsNonArrayMessages(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":"hello"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}

func TestResponsesWithoutFileDoesNotInjectInstruction(t *testing.T) {
	var upstreamPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"Hello"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if _, exists := upstreamPayload["instructions"]; exists {
		t.Fatalf("instructions injected for a file-free request: %#v", upstreamPayload["instructions"])
	}
	if upstreamPayload["input"] != "Hello" {
		t.Fatalf("input = %#v, want unchanged", upstreamPayload["input"])
	}
}

func TestChatWithoutFileDoesNotInjectSystemMessage(t *testing.T) {
	var upstreamPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "chat_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"Hello"}]}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	messages := upstreamPayload["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want the original single message", messages)
	}
	if messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("message = %#v, want unchanged user message", messages[0])
	}
}

func TestResponsesWithFileInjectsInstruction(t *testing.T) {
	var upstreamPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)

	payload := map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "filename": "notes.txt",
				"file_data": base64.StdEncoding.EncodeToString([]byte("document text")),
			}},
		}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	instructions, ok := upstreamPayload["instructions"].(string)
	if !ok || !strings.Contains(instructions, "untrusted content") {
		t.Fatalf("instructions = %#v, want document instruction", upstreamPayload["instructions"])
	}
}

func TestResponsesWithFilePreservesExistingInstructions(t *testing.T) {
	var upstreamPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)

	payload := map[string]any{
		"model":        "test-model",
		"instructions": "Be concise.",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "filename": "notes.txt",
				"file_data": base64.StdEncoding.EncodeToString([]byte("document text")),
			}},
		}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	instructions, ok := upstreamPayload["instructions"].(string)
	if !ok || !strings.Contains(instructions, "untrusted content") || !strings.Contains(instructions, "Be concise.") {
		t.Fatalf("instructions = %#v, want document instruction plus user instructions", upstreamPayload["instructions"])
	}
}

func TestChatWithFilePreservesExistingSystemMessage(t *testing.T) {
	var upstreamPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamPayload); err != nil {
			t.Error(err)
		}
		writeJSON(response, http.StatusOK, map[string]any{"id": "chat_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)

	payload := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "file", "file": map[string]any{
					"file_data": base64.StdEncoding.EncodeToString([]byte("chat document")),
					"filename":  "notes.txt",
				},
			}}},
		},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	messages := upstreamPayload["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v, want document system message plus original messages", messages)
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" || !strings.Contains(system["content"].(string), "untrusted content") {
		t.Fatalf("document system message = %#v", system)
	}
	original := messages[1].(map[string]any)
	if original["role"] != "system" || original["content"] != "You are helpful." {
		t.Fatalf("original system message = %#v", original)
	}
}

func TestOpenAIFileStatusDetails(t *testing.T) {
	base := store.File{
		ID: "file_1", Bytes: 123, CreatedAt: 1000, ExpiresAt: 2000,
		Filename: "notes.txt", Purpose: "user_data",
	}
	for _, test := range []struct {
		name        string
		status      string
		errorMsg    string
		wantStatus  string
		wantDetails any
	}{
		{name: "uploaded", status: "uploaded", wantStatus: "uploaded"},
		{name: "processing", status: "processing", wantStatus: "uploaded"},
		{name: "processed", status: "processed", wantStatus: "processed"},
		{name: "failed_with_message", status: "failed", errorMsg: "document exceeds the 50-page limit", wantStatus: "error", wantDetails: "document exceeds the 50-page limit"},
		{name: "failed_without_message", status: "failed", wantStatus: "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := base
			record.Status = test.status
			if test.errorMsg != "" {
				record.ErrorMessage = sql.NullString{String: test.errorMsg, Valid: true}
			}
			file := openAIFile(record)
			if file["status"] != test.wantStatus {
				t.Fatalf("status = %v, want %v", file["status"], test.wantStatus)
			}
			if details := file["status_details"]; details != test.wantDetails {
				t.Fatalf("status_details = %#v, want %#v", details, test.wantDetails)
			}
			if expiresAt := file["expires_at"]; expiresAt != int64(2000) {
				t.Fatalf("expires_at = %#v, want 2000", expiresAt)
			}
		})
	}
	withoutExpiry := base
	withoutExpiry.Status = "processed"
	withoutExpiry.ExpiresAt = 0
	if expiresAt := openAIFile(withoutExpiry)["expires_at"]; expiresAt != nil {
		t.Fatalf("expires_at = %#v, want nil", expiresAt)
	}
}

func TestFileObjectNullFieldsMatchAcrossEndpoints(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	settings.FileTTL = 0
	handler := NewHandler(settings, dataStore, service)
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	file, err := form.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, "content")
	_ = form.WriteField("purpose", "user_data")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create response = %d: %s", response.Code, response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)

	request = httptest.NewRequest(http.MethodGet, "/v1/files/"+id, nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var retrieved map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &retrieved); err != nil {
		t.Fatal(err)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var listed struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 1 {
		t.Fatalf("listed files = %d, want 1", len(listed.Data))
	}
	for endpoint, fileObject := range map[string]map[string]any{"create": created, "retrieve": retrieved, "list": listed.Data[0]} {
		if fileObject["expires_at"] != nil || fileObject["status_details"] != nil {
			t.Fatalf("%s FileObject = %#v, want null expires_at/status_details", endpoint, fileObject)
		}
	}
}

func TestCreateFileRequiresPurposeWithoutSideEffects(t *testing.T) {
	for _, test := range []struct {
		name         string
		writePurpose bool
	}{
		{name: "missing"},
		{name: "empty", writePurpose: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings, dataStore, service := testDependencies(t)
			handler := NewHandler(settings, dataStore, service)
			body := &bytes.Buffer{}
			form := multipart.NewWriter(body)
			file, err := form.CreateFormFile("file", "notes.txt")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(file, "secret")
			if test.writePurpose {
				_ = form.WriteField("purpose", "")
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
			request.Header.Set("Content-Type", form.FormDataContentType())
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"param":"purpose"`) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			paths, err := dataStore.SourcePaths(context.Background())
			if err != nil || len(paths) != 0 {
				t.Fatalf("persisted paths = %v, %v; want none", paths, err)
			}
			entries, err := os.ReadDir(filepath.Join(settings.DataDir, "files"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("file entries = %v, want none", entries)
			}
		})
	}
}

func TestCreateFileRejectsZipAsUnsupportedFileType(t *testing.T) {
	settings, dataStore, service := testDependencies(t)
	handler := NewHandler(settings, dataStore, service)
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	file, err := form.CreateFormFile("file", "archive.zip")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte{'P', 'K', 3, 4, 0, 0xff})
	_ = form.WriteField("purpose", "user_data")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error.Code != "unsupported_file_type" || result.Error.Message != "unsupported file type" || result.Error.Param != "file" {
		t.Fatalf("error = %#v", result.Error)
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
		MaxDocumentPages: 20, MaxDocumentTextChars: 500_000, Workers: 2,
		VLLMAPIKey: "upstream-key",
	}
	if err := os.Mkdir(filepath.Join(dataDir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := files.New(settings, dataStore, converter.NewDispatcher(converter.NewInTreeRegistry(), converter.DefaultConverterConfig(), nil))
	ctx, cancel := context.WithCancel(context.Background())
	if err := service.Run(ctx, settings.Workers); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		service.Stop()
		_ = dataStore.Close()
	})
	return settings, dataStore, service
}
