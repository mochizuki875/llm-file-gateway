package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/converter"
	"github.com/mochizuki875/llm-file-gateway/internal/files"
	"github.com/mochizuki875/llm-file-gateway/internal/store"
)

type deadlineIgnoringConverter struct {
	ctx           context.Context
	phase         string
	convertCalled chan struct{}
}

func (deadlineIgnoringConverter) Extension() string { return ".deadline" }

func (deadlineIgnoringConverter) MediaType() string { return "text/plain" }

func (documentConverter deadlineIgnoringConverter) Validate(string) error {
	if documentConverter.phase == "validation" {
		<-documentConverter.ctx.Done()
	}
	return nil
}

func (documentConverter deadlineIgnoringConverter) Convert(ctx context.Context, _, _ string, _ converter.Options) (converter.Result, error) {
	documentConverter.convertCalled <- struct{}{}
	<-ctx.Done()
	return converter.Result{}, nil
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
	metadata := content[0].(map[string]any)
	text := content[1].(map[string]any)
	if metadata["type"] != "input_text" || !strings.Contains(metadata["text"].(string), `filename="data.json"`) ||
		text["type"] != "input_text" || !strings.Contains(text["text"].(string), `{"status":"ready"}`) {
		t.Fatalf("expanded content = %#v", content)
	}
	entries, err := os.ReadDir(filepath.Join(settings.DataDir, "work"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary entries = %v, %v", entries, err)
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
			Code    string `json:"code"`
			Message string `json:"message"`
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
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error.Code != "request_too_large" {
		t.Fatalf("error code = %q, want request_too_large", result.Error.Code)
	}
	if result.Error.Message != fmt.Sprintf("Request body exceeds the configured limit of %d bytes.", settings.MaxRequestBodyBytes) {
		t.Fatalf("error message = %q", result.Error.Message)
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

func TestInferenceWithoutFileLeavesJSONValidationToUpstream(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "trailing_whitespace", body: "{\"model\":\"test-model\",\"input\":\"hello\"}\n\t"},
		{name: "second_object", body: `{"model":"test-model","input":"hello"} {}`},
		{name: "second_number", body: `{"model":"test-model","input":"hello"} 123`},
		{name: "trailing_garbage", body: `{"model":"test-model","input":"hello"} garbage`},
		{name: "invalid_json", body: `not json`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var receivedBody string
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				body, _ := io.ReadAll(request.Body)
				receivedBody = string(body)
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
			if response.Code != http.StatusOK || receivedBody != test.body {
				t.Fatalf("status/body = %d/%q, want %d/%q", response.Code, receivedBody, http.StatusOK, test.body)
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
	if result.Error.Code != "request_too_large" {
		t.Fatalf("error code = %q, want request_too_large", result.Error.Code)
	}
}

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
	if result.Error.Code != "request_too_large" {
		t.Fatalf("error code = %q, want request_too_large", result.Error.Code)
	}
}

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

func TestInferenceWaitsForFileWithinRequestDeadline(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions"} {
		for _, test := range []struct {
			name       string
			status     string
			timeout    time.Duration
			wantStatus int
			wantCode   string
		}{
			{name: "uploaded", status: "uploaded", timeout: time.Second, wantStatus: 200},
			{name: "processing", status: "processing", timeout: time.Second, wantStatus: 200},
			{name: "unlimited", status: "uploaded", wantStatus: 200},
			{name: "failed", status: "processing", timeout: time.Second, wantStatus: 422, wantCode: "file_processing_failed"},
			{name: "deleted", status: "uploaded", timeout: time.Second, wantStatus: 404, wantCode: "file_not_found"},
			{name: "timeout", status: "uploaded", timeout: 50 * time.Millisecond, wantStatus: 504, wantCode: "request_timeout"},
			{name: "multiple_files_timeout", status: "uploaded", timeout: 300 * time.Millisecond, wantStatus: 504, wantCode: "request_timeout"},
			// The deadline must outlast the first backoff poll (100ms plus
			// up to 500ms jitter) so the upstream is reached before expiry.
			{name: "upstream_timeout", status: "processing", timeout: time.Second, wantStatus: 504, wantCode: "request_timeout"},
			{name: "canceled", status: "uploaded", timeout: time.Second},
		} {
			t.Run(endpoint+"/"+test.name, func(t *testing.T) {
				settings, dataStore, service := testDependencies(t)
				settings.RequestTimeout = test.timeout
				settings.VLLMBaseURL, _ = url.Parse("http://model.example/v1")
				settings.VLLMModel = "test-model"
				now := time.Now().Unix()
				record := store.File{
					ID: "file_wait", TenantID: store.SharedTenantID, Filename: "notes.txt", MediaType: "text/plain",
					Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: test.status,
					SourcePath: "files/shared/file_wait/source.txt", CreatedAt: now, ExpiresAt: now + 3600,
				}
				if err := dataStore.Add(context.Background(), record); err != nil {
					t.Fatal(err)
				}
				manifestPath := "files/shared/file_wait/manifest.json"
				derivedDir := filepath.Join(settings.DataDir, "files/shared/file_wait/derived")
				if err := os.MkdirAll(derivedDir, 0o700); err != nil {
					t.Fatal(err)
				}
				manifest := converter.Manifest{
					SchemaVersion: converter.ManifestSchemaVersion,
					Documents:     []converter.ManifestDocument{{Name: "notes.txt", Parts: []converter.ManifestPart{{PartNumber: 1, TextPath: "part-0001.txt"}}}},
				}
				encoded, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(settings.DataDir, manifestPath), encoded, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(derivedDir, "part-0001.txt"), []byte("waiting document text"), 0o600); err != nil {
					t.Fatal(err)
				}
				content := make([]any, 0)
				fileIDs := []string{record.ID}
				if test.name == "multiple_files_timeout" {
					other := record
					other.ID = "file_wait_other"
					if err := dataStore.Add(context.Background(), other); err != nil {
						t.Fatal(err)
					}
					fileIDs = append(fileIDs, other.ID)
				}
				for _, id := range fileIDs {
					if endpoint == "responses" {
						content = append(content, map[string]any{"type": "input_file", "file_id": id})
					} else {
						content = append(content, map[string]any{"type": "file", "file": map[string]any{"file_id": id}})
					}
				}
				payload := map[string]any{"model": "test-model"}
				items := []any{map[string]any{"role": "user", "content": content}}
				if endpoint == "responses" {
					payload["input"] = items
				} else {
					payload["messages"] = items
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				var originalDeadline time.Time
				upstreamCalled := make(chan struct{}, 1)
				server := &Server{settings: settings, store: dataStore, files: service}
				server.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					upstreamCalled <- struct{}{}
					if deadline, ok := request.Context().Deadline(); ok {
						t.Errorf("upstream has gateway deadline %v", deadline)
					}
					if test.timeout > 0 && time.Until(originalDeadline) >= test.timeout-20*time.Millisecond {
						t.Error("file wait was not deducted from the upstream time budget")
					}
					forwarded, err := io.ReadAll(request.Body)
					if err != nil || !bytes.Contains(forwarded, []byte("waiting document text")) {
						t.Errorf("upstream payload = %s, error = %v", forwarded, err)
					}
					if test.name == "upstream_timeout" {
						<-request.Context().Done()
						return nil, request.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"response_wait"}`))}, nil
				})}
				started := make(chan struct{})
				handler := server.withRequestTimeout(func(response http.ResponseWriter, request *http.Request) {
					originalDeadline, _ = request.Context().Deadline()
					close(started)
					server.handleInference(response, request, endpoint)
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				request := httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, bytes.NewReader(body)).WithContext(ctx)
				response := httptest.NewRecorder()
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					handler(response, request)
				}()
				<-started
				select {
				case <-finished:
					t.Fatalf("inference returned before file conversion: %s", response.Body.String())
				case <-time.After(20 * time.Millisecond):
				}
				switch test.name {
				case "timeout":
				case "canceled":
					cancel()
				case "deleted":
					if _, err := service.Delete(context.Background(), record.ID, record.TenantID); err != nil {
						t.Fatal(err)
					}
				case "failed":
					if _, err := dataStore.UpdateStatus(context.Background(), record.ID, "failed", "", "conversion failed"); err != nil {
						t.Fatal(err)
					}
				default:
					if _, err := dataStore.UpdateStatus(context.Background(), record.ID, "processed", manifestPath, ""); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Fatal("inference did not finish")
				}
				if test.name == "canceled" {
					if response.Body.Len() != 0 {
						t.Fatalf("canceled request wrote a response: %s", response.Body.String())
					}
				} else if response.Code != test.wantStatus || (test.wantCode != "" && !strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`)) {
					t.Fatalf("response = %d %s, want %d %s", response.Code, response.Body.String(), test.wantStatus, test.wantCode)
				}
				wantUpstream := test.wantStatus == 200 || test.name == "upstream_timeout"
				if got := len(upstreamCalled) != 0; got != wantUpstream {
					t.Fatalf("upstream called = %t, want %t", got, wantUpstream)
				}
			})
		}
	}
}

func TestInlinePreparationDeadlinePreventsInference(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions"} {
		for _, phase := range []string{"validation", "conversion"} {
			t.Run(endpoint+"/"+phase, func(t *testing.T) {
				settings, dataStore, _ := testDependencies(t)
				settings.RequestTimeout = 50 * time.Millisecond
				settings.VLLMModel = "test-model"
				upstreamCalled := make(chan struct{}, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
					upstreamCalled <- struct{}{}
					writeJSON(response, http.StatusOK, map[string]any{"id": "unexpected"})
				}))
				defer upstream.Close()
				settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
				convertCalled := make(chan struct{}, 1)
				registry := converter.Registry{
					".deadline": func(ctx context.Context, _ converter.ConverterConfig, _ converter.ConverterHandle) (converter.DocumentConverter, error) {
						return deadlineIgnoringConverter{ctx: ctx, phase: phase, convertCalled: convertCalled}, nil
					},
				}
				service := files.New(settings, dataStore, converter.NewDispatcher(registry, converter.DefaultConverterConfig(), nil))
				t.Cleanup(service.Stop)
				gateway := httptest.NewServer(NewHandler(settings, dataStore, service))
				defer gateway.Close()
				reference := map[string]any{"filename": "notes.deadline", "file_data": "aGVsbG8="}
				payload := map[string]any{"model": "test-model"}
				if endpoint == "responses" {
					reference["type"] = "input_file"
					payload["input"] = []any{map[string]any{"role": "user", "content": []any{reference}}}
				} else {
					payload["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "file", "file": reference}}}}
				}
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				client := &http.Client{Timeout: 3 * time.Second}
				response, err := client.Post(gateway.URL+"/v1/"+endpoint, "application/json", bytes.NewReader(encoded))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = response.Body.Close() }()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusGatewayTimeout || !bytes.Contains(body, []byte(`"code":"request_timeout"`)) {
					t.Fatalf("response = %d %s, want 504 request_timeout", response.StatusCode, body)
				}
				if len(upstreamCalled) != 0 {
					t.Fatal("expired preparation was forwarded upstream")
				}
				if got, want := len(convertCalled) != 0, phase == "conversion"; got != want {
					t.Fatalf("converter called = %t, want %t", got, want)
				}
				entries, err := os.ReadDir(filepath.Join(settings.DataDir, "work"))
				if err != nil || len(entries) != 0 {
					t.Fatalf("temporary artifacts remained after timeout: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestFileReferenceIDs(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		payload  map[string]any
	}{
		{
			name: "responses", endpoint: "responses",
			payload: map[string]any{"input": []any{map[string]any{"content": []any{
				map[string]any{"type": "input_file", "file_id": "file_a"},
				map[string]any{"type": "input_file", "file_data": "inline"},
				map[string]any{"type": "input_file", "file_id": "file_a"},
				map[string]any{"type": "input_file", "file_id": "file_b"},
			}}}},
		},
		{
			name: "chat", endpoint: "chat/completions",
			payload: map[string]any{"messages": []any{map[string]any{"content": []any{
				map[string]any{"type": "file", "file": map[string]any{"file_id": "file_a"}},
				map[string]any{"type": "file", "file": map[string]any{"file_data": "inline"}},
				map[string]any{"type": "file", "file": map[string]any{"file_id": "file_b"}},
			}}}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := strings.Join(fileReferenceIDs(test.endpoint, test.payload), ","); got != "file_a,file_b" {
				t.Fatalf("fileReferenceIDs() = %q, want %q", got, "file_a,file_b")
			}
		})
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
	metadata := content[0].(map[string]any)
	text := content[1].(map[string]any)
	if metadata["type"] != "text" || !strings.Contains(metadata["text"].(string), `filename="notes.txt"`) ||
		text["type"] != "text" || !strings.Contains(text["text"].(string), "chat document") {
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

func TestChatWithoutFileLeavesValidationToUpstream(t *testing.T) {
	const body = `{"model":"test-model","messages":"hello"}`
	var receivedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		content, _ := io.ReadAll(request.Body)
		receivedBody = string(content)
		writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
	}))
	defer upstream.Close()
	settings, dataStore, service := testDependencies(t)
	settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
	settings.VLLMModel = "test-model"
	handler := NewHandler(settings, dataStore, service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || receivedBody != body {
		t.Fatalf("status/body = %d/%q, want %d/%q", response.Code, receivedBody, http.StatusOK, body)
	}
}

func TestInferenceWithoutFilePassesThroughRequest(t *testing.T) {
	for _, test := range []struct {
		name, endpoint, body string
	}{
		{"responses", "/v1/responses?include=reasoning", `{"model":"any-model","input":[{"role":"user","content":[{"type":"input_text","text":"Hello"}]}],"custom":{"number":1.0e+10}}`},
		{"chat", "/v1/chat/completions?n=2", `{"model":"any-model","messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]}],"custom":{"number":1.0e+10}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var receivedBody, receivedQuery string
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				body, _ := io.ReadAll(request.Body)
				receivedBody = string(body)
				receivedQuery = request.URL.RawQuery
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
			if receivedBody != test.body || receivedQuery != request.URL.RawQuery {
				t.Fatalf("upstream body/query = %q/%q, want %q/%q", receivedBody, receivedQuery, test.body, request.URL.RawQuery)
			}
		})
	}
}

func TestInferenceRequestBodyLimit(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions"} {
		for _, test := range []struct {
			name   string
			body   string
			limit  int64
			status int
		}{
			{name: "below_limit", body: `{}`, limit: 3, status: http.StatusOK},
			{name: "exact_limit", body: `{}`, limit: 2, status: http.StatusOK},
			{name: "over_limit", body: `{}`, limit: 1, status: http.StatusBadRequest},
			{name: "invalid_json_over_limit", body: `not JSON`, limit: 1, status: http.StatusBadRequest},
			{name: "invalid_json_within_limit", body: `not JSON`, limit: 8, status: http.StatusOK},
			{name: "unlimited", body: `{}`, limit: 0, status: http.StatusOK},
			{name: "stop_reading_early", body: strings.Repeat("a", 1<<20), limit: 16, status: http.StatusBadRequest},
		} {
			t.Run(endpoint+"/"+test.name, func(t *testing.T) {
				var receivedBody string
				upstreamCalled := false
				upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
					body, _ := io.ReadAll(request.Body)
					receivedBody = string(body)
					upstreamCalled = true
					writeJSON(response, http.StatusOK, map[string]any{"id": "response_1"})
				}))
				defer upstream.Close()
				settings, dataStore, service := testDependencies(t)
				settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
				settings.MaxRequestBodyBytes = test.limit
				handler := NewHandler(settings, dataStore, service)
				reader := strings.NewReader(test.body)
				request := httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, reader)
				request.ContentLength = -1
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != test.status {
					t.Fatalf("status = %d: %s", response.Code, response.Body.String())
				}
				if test.status == http.StatusOK {
					if !upstreamCalled || receivedBody != test.body {
						t.Fatalf("upstream called = %t, body = %q, want %q", upstreamCalled, receivedBody, test.body)
					}
				} else if upstreamCalled || !strings.Contains(response.Body.String(), `"code":"request_too_large"`) {
					t.Fatalf("upstream called = %t, response = %s", upstreamCalled, response.Body.String())
				}
				if test.name == "stop_reading_early" && reader.Len() != len(test.body)-int(test.limit)-1 {
					t.Fatalf("consumed %d bytes, want %d", len(test.body)-reader.Len(), test.limit+1)
				}
			})
		}
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
