package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/logging"
)

var hopByHopHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true, "host": true, "content-length": true,
}

const upstreamErrorPreviewBytes = 64 << 10

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
	upstreamURL := strings.TrimRight(server.settings.VLLMBaseURL.String(), "/") + "/" + path
	if request.URL.RawQuery != "" {
		upstreamURL += "?" + request.URL.RawQuery
	}
	upstreamRequest, err := http.NewRequestWithContext(request.Context(), request.Method, upstreamURL, request.Body)
	if err != nil {
		slog.Error("upstream request creation failed", "method", request.Method, "path", request.URL.Path, "error", err)
		writeError(response, apierror.New(500, "internal_error", "Internal server error.", ""))
		return
	}
	copyHeaders(upstreamRequest.Header, request.Header)
	upstreamRequest.Header.Del("Authorization")
	if authorization := server.upstreamAuthorization(request); authorization != "" {
		upstreamRequest.Header.Set("Authorization", authorization)
	}
	logging.V(request.Context(), 1, "forwarding upstream request", "method", request.Method, "path", request.URL.Path)
	upstream, err := server.client.Do(upstreamRequest)
	if err != nil {
		slog.Error("upstream request failed", "method", request.Method, "path", request.URL.Path, "error", safeHTTPError(err))
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			writeError(response, apierror.New(504, "model_timeout", "The model request timed out.", ""))
		} else {
			writeError(response, apierror.New(502, "model_upstream_error", "Unable to reach the model server.", ""))
		}
		return
	}
	defer upstream.Body.Close()
	upstreamBody := server.logUpstreamErrorResponse(upstream, "upstream returned error", "method", request.Method, "path", request.URL.Path)
	copyHeaders(response.Header(), upstream.Header)
	response.WriteHeader(upstream.StatusCode)
	if _, err := io.Copy(response, upstreamBody); err != nil {
		slog.Debug("upstream response copy interrupted", "method", request.Method, "path", request.URL.Path, "error", err)
	}
}

func copyHeaders(destination, source http.Header) {
	for name, values := range source {
		if hopByHopHeaders[strings.ToLower(name)] {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func (server *Server) forwardJSON(response http.ResponseWriter, request *http.Request, endpoint string, payload map[string]any) {
	content, _ := json.Marshal(payload)
	upstreamURL := strings.TrimRight(server.settings.VLLMBaseURL.String(), "/") + "/" + endpoint
	upstreamRequest, _ := http.NewRequestWithContext(request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(content))
	upstreamRequest.Header.Set("Content-Type", "application/json")
	if authorization := server.upstreamAuthorization(request); authorization != "" {
		upstreamRequest.Header.Set("Authorization", authorization)
	}
	logging.V(request.Context(), 1, "forwarding inference request", "endpoint", endpoint)
	upstream, err := server.client.Do(upstreamRequest)
	if err != nil {
		slog.Error("upstream inference request failed", "endpoint", endpoint, "error", safeHTTPError(err))
		writeError(response, apierror.New(502, "model_upstream_error", "Unable to reach the model server.", ""))
		return
	}
	defer upstream.Body.Close()
	upstreamBody := server.logUpstreamErrorResponse(upstream, "upstream inference returned error", "endpoint", endpoint)
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

func (server *Server) upstreamAuthorization(request *http.Request) string {
	if server.settings.GatewayAuthRequired {
		return "Bearer " + server.settings.VLLMAPIKey
	}
	return request.Header.Get("Authorization")
}

func safeHTTPError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}
