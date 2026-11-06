package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/config"
)

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
			if strings.Contains(test.name, "without_file") {
				if got := receivedHeader.Get("Content-Type"); got != "text/plain" {
					t.Errorf("upstream Content-Type = %q, want text/plain", got)
				}
				if receivedLength != -1 {
					t.Errorf("upstream Content-Length = %d, want unknown length", receivedLength)
				}
			} else {
				if got := receivedHeader.Get("Content-Type"); got != "application/json" {
					t.Errorf("upstream Content-Type = %q", got)
				}
				if receivedLength != int64(len(receivedBody)) || receivedLength == 1 {
					t.Errorf("upstream Content-Length = %d, body length = %d", receivedLength, len(receivedBody))
				}
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
		{name: "passthrough_completions", endpoint: "completions", payload: `{"model":"test-model","prompt":"hello","stream":true}`},
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
			defer func() {
				releaseOnce.Do(func() { close(releaseSecondEvent) })
				upstream.Close()
			}()

			settings, dataStore, service := testDependencies(t)
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			settings.VLLMModel = "test-model"
			gateway := httptest.NewServer(NewHandler(settings, dataStore, service))
			defer func() {
				releaseOnce.Do(func() { close(releaseSecondEvent) })
				gateway.Close()
			}()

			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Post(
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

func TestResponseContinuesAfterRequestTimeout(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		payload  string
	}{
		{name: "responses", endpoint: "/v1/responses", payload: `{"model":"test-model","stream":true,"input":"hello"}`},
		{name: "chat", endpoint: "/v1/chat/completions", payload: `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`},
		{name: "responses_inline", endpoint: "/v1/responses", payload: `{"model":"test-model","stream":true,"input":[{"role":"user","content":[{"type":"input_file","filename":"notes.txt","file_data":"aGVsbG8="}]}]}`},
		{name: "chat_inline", endpoint: "/v1/chat/completions", payload: `{"model":"test-model","stream":true,"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"notes.txt","file_data":"aGVsbG8="}}]}]}`},
		{name: "nonstream", endpoint: "/v1/responses", payload: `{"model":"test-model","input":"hello"}`},
		{name: "nonstream_inline", endpoint: "/v1/responses", payload: `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_file","filename":"notes.txt","file_data":"aGVsbG8="}]}]}`},
		{name: "passthrough", endpoint: "/v1/embeddings", payload: `{"input":"hello"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			releaseResponse := make(chan struct{})
			upstreamFinished := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v1/models" {
					response.WriteHeader(http.StatusOK)
					return
				}
				defer close(upstreamFinished)
				response.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(response, "data: {}\n\n")
				_ = http.NewResponseController(response).Flush()
				select {
				case <-releaseResponse:
					_, _ = io.WriteString(response, "data: [DONE]\n\n")
				case <-request.Context().Done():
				}
			}))
			defer upstream.Close()
			settings, dataStore, service := testDependencies(t)
			settings.RequestTimeout = 200 * time.Millisecond
			settings.MaxConcurrentRequests = 1
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			settings.VLLMModel = "test-model"
			gateway := httptest.NewServer(NewHandler(settings, dataStore, service))
			defer gateway.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+test.endpoint, strings.NewReader(test.payload))
			if err != nil {
				t.Fatal(err)
			}
			type responseResult struct {
				status int
				body   string
				err    error
			}
			finished := make(chan responseResult, 1)
			responseStarted := make(chan struct{})
			go func() {
				client := &http.Client{Timeout: 3 * time.Second}
				response, err := client.Do(request)
				if err != nil {
					finished <- responseResult{err: err}
					return
				}
				defer func() { _ = response.Body.Close() }()
				close(responseStarted)
				body, err := io.ReadAll(response.Body)
				finished <- responseResult{status: response.StatusCode, body: string(body), err: err}
			}()
			if strings.Contains(test.payload, `"stream":true`) {
				select {
				case <-responseStarted:
				case <-time.After(3 * time.Second):
					t.Fatal("stream response headers not received")
				}
				client := &http.Client{Timeout: 3 * time.Second}
				probe, err := client.Get(gateway.URL + "/v1/models")
				if err != nil {
					t.Fatal(err)
				}
				_ = probe.Body.Close()
				if probe.StatusCode != http.StatusOK {
					t.Fatalf("request during active stream = %d, want 200", probe.StatusCode)
				}
			}
			select {
			case result := <-finished:
				t.Fatalf("response stopped before upstream completion: %+v", result)
			case <-time.After(2 * settings.RequestTimeout):
			}
			close(releaseResponse)
			select {
			case result := <-finished:
				if result.err != nil || result.status != http.StatusOK || result.body != "data: {}\n\ndata: [DONE]\n\n" {
					t.Fatalf("response = %+v, want the complete response", result)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upstream completion did not finish response forwarding")
			}
			select {
			case <-upstreamFinished:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream handler did not finish")
			}
		})
	}
}

func TestForwardJSONRejectsExpiredPreparation(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	server := &Server{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("expired preparation was forwarded upstream")
		return nil, errors.New("unexpected upstream call")
	})}}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	server.forwardJSON(response, request, "responses", map[string]any{"model": "test-model"}, nil)
	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), `"code":"request_timeout"`) {
		t.Fatalf("response = %d %s, want 504 request_timeout", response.Code, response.Body.String())
	}
}

func TestForwardJSONLogsUpstreamRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{}`)
	}))
	defer upstream.Close()
	baseURL, err := url.Parse(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{settings: config.Config{VLLMBaseURL: baseURL}, client: upstream.Client()}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	response := httptest.NewRecorder()

	previousLogger := slog.Default()
	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	server.forwardJSON(response, request, "responses", map[string]any{"model": "secret-model-value"}, []string{"file_example"})
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	logOutput := output.String()
	for _, expected := range []string{`msg="forwarding upstream request"`, "method=POST", "path=/v1/responses", "endpoint=responses", "file_ids=[file_example]"} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf("log output = %q, missing %q", logOutput, expected)
		}
	}
	if strings.Contains(logOutput, "secret-model-value") {
		t.Fatalf("log output contains request payload: %q", logOutput)
	}
}

func TestResponseContextPreservesCallerDeadline(t *testing.T) {
	callerContext, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	server := &Server{settings: config.Config{RequestTimeout: 20 * time.Millisecond}}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(callerContext)
	server.withRequestTimeout(func(response http.ResponseWriter, request *http.Request) {
		upstreamContext, startResponse, cleanup := responseContext(response, request)
		defer cleanup()
		if err := startResponse(); err != nil {
			t.Fatal(err)
		}
		gotDeadline, ok := upstreamContext.Deadline()
		wantDeadline, _ := callerContext.Deadline()
		if !ok || !gotDeadline.Equal(wantDeadline) {
			t.Fatalf("upstream deadline = %v, want caller deadline %v", gotDeadline, wantDeadline)
		}
		<-request.Context().Done()
		if err := upstreamContext.Err(); err != nil {
			t.Fatalf("gateway timeout canceled response context: %v", err)
		}
		select {
		case <-upstreamContext.Done():
			if !errors.Is(upstreamContext.Err(), context.DeadlineExceeded) {
				t.Fatalf("upstream error = %v, want caller deadline exceeded", upstreamContext.Err())
			}
		case <-time.After(time.Second):
			t.Fatal("caller deadline did not cancel response context")
		}
	})(httptest.NewRecorder(), request)
}

func TestSlowClientResponseContinuesAfterRequestTimeout(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "original", payload: `{"model":"test-model","stream":true,"input":"hello"}`},
		{name: "expanded", payload: `{"model":"test-model","stream":true,"input":[{"role":"user","content":[{"type":"input_file","filename":"notes.txt","file_data":"aGVsbG8="}]}]}`},
		{name: "nonstream", payload: `{"model":"test-model","input":"hello"}`},
		{name: "nonstream_expanded", payload: `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_file","filename":"notes.txt","file_data":"aGVsbG8="}]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				chunk := bytes.Repeat([]byte("x"), 64*1024)
				for range 1024 {
					if request.Context().Err() != nil {
						return
					}
					if _, err := response.Write(chunk); err != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			settings, dataStore, service := testDependencies(t)
			settings.RequestTimeout = 200 * time.Millisecond
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			settings.VLLMModel = "test-model"
			handler := NewHandler(settings, dataStore, service)
			finished := make(chan struct{})
			gateway := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				defer close(finished)
				handler.ServeHTTP(response, request)
			}))
			defer gateway.Close()
			connection, err := net.DialTCP("tcp", nil, gateway.Listener.Addr().(*net.TCPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close() }()
			if err := connection.SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if _, err := fmt.Fprintf(connection, "POST /v1/responses HTTP/1.1\r\nHost: gateway\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(test.payload), test.payload); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(connection), nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			select {
			case <-finished:
				t.Fatal("response stopped at the gateway deadline")
			case <-time.After(time.Until(started.Add(2 * settings.RequestTimeout))):
			}
			_ = connection.Close()
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("client disconnection did not finish response forwarding")
			}
		})
	}
}

func TestUpstreamErrorBodyTimeoutReturnsRequestTimeout(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		payload  string
	}{
		{name: "responses", endpoint: "/v1/responses", payload: `{"model":"test-model","input":"hello"}`},
		{name: "chat", endpoint: "/v1/chat/completions", payload: `{"model":"test-model","messages":[]}`},
		{name: "inline", endpoint: "/v1/responses", payload: `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_file","filename":"notes.txt","file_data":"aGVsbG8="}]}]}`},
		{name: "passthrough", endpoint: "/v1/embeddings", payload: `{"input":"hello"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("X-Upstream-Error", "true")
				response.WriteHeader(http.StatusBadRequest)
				_ = http.NewResponseController(response).Flush()
				<-request.Context().Done()
			}))
			defer upstream.Close()
			settings, dataStore, service := testDependencies(t)
			settings.RequestTimeout = 100 * time.Millisecond
			settings.VLLMBaseURL, _ = url.Parse(upstream.URL + "/v1")
			settings.VLLMModel = "test-model"
			gateway := httptest.NewServer(NewHandler(settings, dataStore, service))
			defer gateway.Close()
			client := &http.Client{Timeout: 3 * time.Second}
			response, err := client.Post(gateway.URL+test.endpoint, "application/json", strings.NewReader(test.payload))
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
			if response.Header.Get("X-Upstream-Error") != "" {
				t.Fatal("timed-out upstream headers were forwarded")
			}
		})
	}
}

func TestUpstreamTimeoutReturnsRequestTimeout(t *testing.T) {
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
			if result.Error.Code != "request_timeout" {
				t.Fatalf("error code = %q, want request_timeout", result.Error.Code)
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
