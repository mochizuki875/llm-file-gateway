package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
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

func TestConcurrencyLimitReleasesSlot(t *testing.T) {
	for _, mode := range []string{"headers", "implicit", "error", "return", "panic"} {
		t.Run(mode, func(t *testing.T) {
			server := &Server{slots: make(chan struct{}, 1)}
			entered := make(chan struct{})
			proceed := make(chan struct{})
			started := make(chan struct{})
			finish := make(chan struct{})
			done := make(chan struct{})
			defer func() {
				close(finish)
				<-done
			}()
			handler := server.withConcurrencyLimit(func(response http.ResponseWriter, _ *http.Request) {
				close(entered)
				<-proceed
				switch mode {
				case "return":
					return
				case "panic":
					panic("test panic")
				case "implicit":
					_, _ = response.Write([]byte("body"))
				case "error":
					response.WriteHeader(http.StatusBadRequest)
				default:
					response.WriteHeader(http.StatusEarlyHints)
					if len(server.slots) != 1 {
						t.Error("informational headers released the slot")
					}
					response.WriteHeader(http.StatusOK)
				}
				close(started)
				<-finish
			})
			go func() {
				defer close(done)
				defer func() { _ = recover() }()
				handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
			}()
			<-entered
			probe := server.withConcurrencyLimit(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(http.StatusNoContent)
			})
			rejected := httptest.NewRecorder()
			probe(rejected, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
			close(proceed)
			if rejected.Code != http.StatusTooManyRequests || !strings.Contains(rejected.Body.String(), `"code":"too_many_requests"`) {
				t.Fatalf("rejection = %d %s", rejected.Code, rejected.Body.String())
			}
			select {
			case <-started:
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not start its response or finish")
			}
			accepted := httptest.NewRecorder()
			probe(accepted, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
			if accepted.Code != http.StatusNoContent {
				t.Fatalf("status after release = %d", accepted.Code)
			}
		})
	}
}

func TestConcurrencyLimitRoutes(t *testing.T) {
	for _, limit := range []int{0, 1} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			settings, dataStore, service := testDependencies(t)
			settings.MaxConcurrentRequests = limit
			entered := make(chan struct{})
			proceed := make(chan struct{})
			done := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Query().Has("hold") {
					close(entered)
					<-proceed
				}
				response.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			handler := NewHandler(settings, dataStore, service)
			go func() {
				defer close(done)
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses?hold", strings.NewReader(`{}`)))
			}()
			defer func() {
				close(proceed)
				<-done
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream request not received")
			}
			for _, route := range []struct {
				method string
				path   string
				status int
			}{
				{http.MethodPost, "/v1/responses", http.StatusOK},
				{http.MethodPost, "/v1/chat/completions", http.StatusOK},
				{http.MethodGet, "/v1/models", http.StatusOK},
				{http.MethodGet, "/health", http.StatusOK},
				{http.MethodGet, "/v1/files", http.StatusOK},
				{http.MethodPatch, "/v1/files/missing", http.StatusMethodNotAllowed},
			} {
				want := route.status
				if limit > 0 && route.path != "/health" {
					want = http.StatusTooManyRequests
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`)))
				if response.Code != want {
					t.Errorf("%s %s: status = %d, want %d: %s", route.method, route.path, response.Code, want, response.Body.String())
				}
			}
		})
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

func TestRequestTimeoutDuringBodyRead(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions"} {
		t.Run(endpoint, func(t *testing.T) {
			settings, dataStore, service := testDependencies(t)
			settings.RequestTimeout = 50 * time.Millisecond
			gateway := httptest.NewServer(NewHandler(settings, dataStore, service))
			defer gateway.Close()
			connection, err := net.Dial("tcp", gateway.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close() }()
			if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(connection, "POST /v1/%s HTTP/1.1\r\nHost: gateway\r\nContent-Length: 1024\r\nContent-Type: application/json\r\n\r\n{", endpoint); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(connection), nil)
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
		})
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
		{name: "failed_with_message", status: "failed", errorMsg: "document exceeds the 50-page limit", wantStatus: "error", wantDetails: `File processing failed for "notes.txt".`},
		{name: "failed_with_internal_path", status: "failed", errorMsg: "could not render /tmp/document-image-renderer-123/source.pdf", wantStatus: "error", wantDetails: `File processing failed for "notes.txt".`},
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

func TestWriteErrorType(t *testing.T) {
	for _, test := range []struct {
		status int
		kind   string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusInternalServerError, "server_error"},
		{http.StatusBadGateway, "server_error"},
		{http.StatusGatewayTimeout, "server_error"},
	} {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			response := httptest.NewRecorder()
			writeError(response, apierror.New(test.status, "test", "test error", ""))
			if response.Code != test.status || !strings.Contains(response.Body.String(), `"type":"`+test.kind+`"`) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestCreateFileRejectsOversizedUpload(t *testing.T) {
	for _, size := range []int{1025, 1024 + (2 << 20)} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			settings, dataStore, service := testDependencies(t)
			handler := NewHandler(settings, dataStore, service)
			body := &bytes.Buffer{}
			form := multipart.NewWriter(body)
			file, err := form.CreateFormFile("file", "large.txt")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = file.Write(bytes.Repeat([]byte("a"), size))
			_ = form.WriteField("purpose", "user_data")
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/files", body)
			request.Header.Set("Content-Type", form.FormDataContentType())
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"file_too_large"`) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			paths, err := dataStore.SourcePaths(context.Background())
			if err != nil || len(paths) != 0 {
				t.Fatalf("persisted paths = %v, %v; want none", paths, err)
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
