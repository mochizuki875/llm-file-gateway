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
	settings.MaxDocumentImages = 8
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
	settings.MaxDocumentImages = 1
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
	if receivedRequestID != "client-1" || receivedAuthorization != "Bearer upstream-key" {
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
