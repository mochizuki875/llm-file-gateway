package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/logging"
)

// hopByHopHeaders are connection-specific headers that must not be forwarded
// to the upstream server.
var hopByHopHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "proxy-connection": true, "te": true,
	"trailer": true, "transfer-encoding": true, "upgrade": true,
	"host": true, "content-length": true,
}

// upstreamErrorPreviewBytes is the maximum number of upstream error body bytes
// that are logged.
const upstreamErrorPreviewBytes = 64 << 10

// passthrough forwards any /v1/* request that is not handled by the gateway
// itself to the vLLM server, replacing the client Authorization header with
// the upstream API key.
func (server *Server) passthrough(response http.ResponseWriter, request *http.Request) {
	if _, gatewayError := server.tenantID(request); gatewayError != nil {
		writeError(response, gatewayError)
		return
	}
	path := request.PathValue("path")
	if path == "files" || strings.HasPrefix(path, "files/") {
		writeError(response, apierror.New(405, "method_not_allowed", "Method not allowed for the Files API.", ""))
		return
	}
	server.forwardRequest(response, request, path, false, false)
}

// forwardRequest forwards a request body without transforming it. Callers are
// responsible for authenticating gateway clients before invoking it.
func (server *Server) forwardRequest(response http.ResponseWriter, request *http.Request, path string, stream, inference bool) {
	upstreamContext, startResponse, cleanup := responseContext(response, request)
	defer cleanup()
	upstreamURL := strings.TrimRight(server.settings.VLLMBaseURL.String(), "/") + "/" + path
	if request.URL.RawQuery != "" {
		upstreamURL += "?" + request.URL.RawQuery
	}
	upstreamRequest, err := http.NewRequestWithContext(upstreamContext, request.Method, upstreamURL, request.Body)
	if err != nil {
		slog.Error("upstream request creation failed", "method", request.Method, "path", request.URL.Path, "error", err)
		writeError(response, apierror.New(500, "internal_error", "Internal server error.", ""))
		return
	}
	copyHeaders(upstreamRequest.Header, request.Header)
	upstreamRequest.Header.Del("Authorization")
	if authorization := server.upstreamAuthorization(); authorization != "" {
		upstreamRequest.Header.Set("Authorization", authorization)
	}
	logging.V(request.Context(), 1, "forwarding upstream request", "method", request.Method, "path", request.URL.Path)
	upstream, err := server.client.Do(upstreamRequest)
	if err != nil {
		if contextError := request.Context().Err(); contextError != nil {
			err = contextError
		}
		slog.Error("upstream request failed", "method", request.Method, "path", request.URL.Path, "error", safeHTTPError(err))
		writeError(response, upstreamError(err))
		return
	}
	defer func() { _ = upstream.Body.Close() }()
	message := "upstream returned error"
	attributes := []any{"method", request.Method, "path", request.URL.Path}
	if inference {
		message = "upstream inference returned error"
		attributes = []any{"endpoint", path}
	}
	upstreamBody := server.logUpstreamErrorResponse(upstream, message, attributes...)
	if err := startResponse(); err != nil {
		writeError(response, upstreamError(err))
		return
	}
	copyHeaders(response.Header(), upstream.Header)
	response.WriteHeader(upstream.StatusCode)
	if stream || strings.EqualFold(strings.TrimSpace(strings.SplitN(upstream.Header.Get("Content-Type"), ";", 2)[0]), "text/event-stream") {
		if _, err := io.Copy(flushWriter{response: response, controller: http.NewResponseController(response)}, upstreamBody); err != nil {
			slog.Debug("upstream stream copy interrupted", "endpoint", path, "error", err)
		}
		return
	}
	if _, err := io.Copy(response, upstreamBody); err != nil {
		slog.Debug("upstream response copy interrupted", "method", request.Method, "path", request.URL.Path, "error", err)
	}
}

// copyHeaders copies all non-hop-by-hop headers from source to destination.
// In addition to the fixed hop-by-hop list, headers named in the Connection
// header are removed because they are connection-specific for this hop and
// must not be forwarded to the upstream server.
func copyHeaders(destination, source http.Header) {
	connectionHeaders := make(map[string]bool)
	for _, value := range source.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				connectionHeaders[strings.ToLower(name)] = true
			}
		}
	}
	for name, values := range source {
		lower := strings.ToLower(name)
		if hopByHopHeaders[lower] || connectionHeaders[lower] {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

// forwardJSON posts an expanded inference payload to the vLLM endpoint and
// streams the response back to the client, flushing after each write when the
// request is a streaming one.
func (server *Server) forwardJSON(response http.ResponseWriter, request *http.Request, endpoint string, payload map[string]any) {
	content, _ := json.Marshal(payload)
	if err := request.Context().Err(); err != nil {
		writeAnyError(response, request, err)
		return
	}
	upstreamContext, startResponse, cleanup := responseContext(response, request)
	defer cleanup()

	upstreamURL := strings.TrimRight(server.settings.VLLMBaseURL.String(), "/") + "/" + endpoint
	upstreamRequest, _ := http.NewRequestWithContext(upstreamContext, http.MethodPost, upstreamURL, bytes.NewReader(content))
	copyHeaders(upstreamRequest.Header, request.Header)
	upstreamRequest.Header.Del("Authorization")
	upstreamRequest.Header.Set("Content-Type", "application/json")
	if authorization := server.upstreamAuthorization(); authorization != "" {
		upstreamRequest.Header.Set("Authorization", authorization)
	}
	upstream, err := server.client.Do(upstreamRequest)
	if err != nil {
		if contextError := request.Context().Err(); contextError != nil {
			err = contextError
		}
		slog.Error("upstream inference request failed", "endpoint", endpoint, "error", safeHTTPError(err))
		writeError(response, upstreamError(err))
		return
	}
	defer func() { _ = upstream.Body.Close() }()
	upstreamBody := server.logUpstreamErrorResponse(upstream, "upstream inference returned error", "endpoint", endpoint)
	if err := startResponse(); err != nil {
		writeError(response, upstreamError(err))
		return
	}
	copyHeaders(response.Header(), upstream.Header)
	response.WriteHeader(upstream.StatusCode)
	if stream, _ := payload["stream"].(bool); stream {
		if _, err := io.Copy(flushWriter{response: response, controller: http.NewResponseController(response)}, upstreamBody); err != nil {
			slog.Debug("upstream stream copy interrupted", "endpoint", endpoint, "error", err)
		}
		return
	}
	if _, err := io.Copy(response, upstreamBody); err != nil {
		slog.Debug("upstream response copy interrupted", "endpoint", endpoint, "error", err)
	}
}

func responseContext(response http.ResponseWriter, request *http.Request) (context.Context, func() error, func()) {
	clientContext, ok := request.Context().Value(clientContextKey{}).(context.Context)
	if !ok {
		clientContext = request.Context()
	}
	upstreamContext, cancel := context.WithCancel(clientContext)
	stopTimeout := context.AfterFunc(request.Context(), cancel)
	startResponse := func() error {
		stopTimeout()
		if err := request.Context().Err(); err != nil {
			return err
		}
		controller := http.NewResponseController(response)
		deadline := time.Time{}
		if clientDeadline, ok := clientContext.Deadline(); ok {
			deadline = clientDeadline
		}
		_ = controller.SetReadDeadline(deadline)
		_ = controller.SetWriteDeadline(deadline)
		return nil
	}
	return upstreamContext, startResponse, func() {
		stopTimeout()
		cancel()
	}
}

// upstreamError maps an upstream request failure to a gateway error. Timeouts
// share the 504 request_timeout status and code with preparation timeouts and
// are distinguished by the error message; all other failures are 502
// model_upstream_error.
func upstreamError(err error) *apierror.Error {
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return apierror.New(504, "request_timeout", "The request to upstream model timeout.", "")
	}
	return apierror.New(502, "model_upstream_error", "Unable to reach the model server.", "")
}

// logUpstreamErrorResponse logs a preview of upstream error responses (without
// logging file contents) and returns a reader that replays the full body.
func (server *Server) logUpstreamErrorResponse(upstream *http.Response, message string, attributes ...any) io.Reader {
	if upstream.StatusCode < http.StatusBadRequest {
		return upstream.Body
	}
	preview, err := io.ReadAll(io.LimitReader(upstream.Body, upstreamErrorPreviewBytes+1))
	body := io.MultiReader(bytes.NewReader(preview), upstream.Body)
	attributes = append(attributes, "status", upstream.StatusCode)
	if err != nil {
		slog.Error(message, append(attributes, "error", err)...)
		return body
	}
	if len(preview) > upstreamErrorPreviewBytes {
		slog.Error(message, attributes...)
		return body
	}
	var result struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   any    `json:"param"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(preview, &result); err == nil {
		attributes = append(attributes,
			"error_type", result.Error.Type,
			"error_code", result.Error.Code,
			"error_param", result.Error.Param,
			"error_message", result.Error.Message,
		)
	}
	slog.Error(message, attributes...)
	return body
}

// flushWriter flushes the response after every write so that streaming
// responses reach the client immediately.
type flushWriter struct {
	response   http.ResponseWriter
	controller *http.ResponseController
}

func (writer flushWriter) Write(content []byte) (int, error) {
	written, err := writer.response.Write(content)
	if err != nil {
		return written, err
	}
	if err := writer.controller.Flush(); err != nil {
		return written, err
	}
	return written, nil
}

// upstreamAuthorization returns the bearer token used to authenticate with vLLM.
func (server *Server) upstreamAuthorization() string {
	return "Bearer " + server.settings.VLLMAPIKey
}

// safeHTTPError unwraps url.Error so that the underlying network error is
// logged without the full URL.
func safeHTTPError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}
