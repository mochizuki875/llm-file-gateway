package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
)

// downloadPublicHTTPS downloads a file from a public HTTPS URL, following up
// to four redirects and re-validating each one. It returns the content and a
// filename derived from the final URL path.
func (server *Server) downloadPublicHTTPS(ctx context.Context, rawURL, param string) ([]byte, string, error) {
	parsed, err := server.validatePublicHTTPS(ctx, rawURL, param)
	if err != nil {
		return nil, "", err
	}
	client := *server.client
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	current := parsed
	for range 4 {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		upstream, err := client.Do(request)
		if err != nil {
			slog.Warn("file URL download failed", "host", current.Hostname(), "error", safeHTTPError(err))
			return nil, "", apierror.New(400, "invalid_file_url", "Unable to download file URL.", param)
		}
		if upstream.StatusCode >= 300 && upstream.StatusCode < 400 {
			location := upstream.Header.Get("Location")
			upstream.Body.Close()
			next, err := current.Parse(location)
			if err != nil {
				break
			}
			current, err = server.validatePublicHTTPS(ctx, next.String(), param)
			if err != nil {
				return nil, "", err
			}
			continue
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			upstream.Body.Close()
			slog.Warn("file URL returned non-success status", "host", current.Hostname(), "status", upstream.StatusCode)
			return nil, "", apierror.New(400, "invalid_file_url", "Unable to download file URL.", param)
		}
		content, err := io.ReadAll(io.LimitReader(upstream.Body, server.settings.MaxFileBytes+1))
		upstream.Body.Close()
		if err != nil {
			slog.Warn("file URL response read failed", "host", current.Hostname(), "error", err)
			return nil, "", err
		}
		return content, filepath.Base(current.Path), nil
	}
	return nil, "", apierror.New(400, "invalid_file_url", "Too many redirects.", param)
}

// validatePublicHTTPS enforces that a URL is HTTPS on port 443 without user
// info and that its hostname resolves only to public IP addresses, preventing
// SSRF to internal networks.
func (server *Server) validatePublicHTTPS(ctx context.Context, rawURL, param string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "443") {
		return nil, apierror.New(400, "invalid_file_url", "Only public HTTPS URLs on port 443 are allowed.", param)
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, parsed.Hostname())
	if err != nil || len(addresses) == 0 {
		slog.Warn("file URL hostname resolution failed", "host", parsed.Hostname(), "error", err)
		return nil, apierror.New(400, "invalid_file_url", "File URL hostname cannot be resolved.", param)
	}
	for _, address := range addresses {
		if address.IP.IsPrivate() || address.IP.IsLoopback() || address.IP.IsLinkLocalUnicast() || address.IP.IsUnspecified() || address.IP.IsMulticast() {
			return nil, apierror.New(400, "invalid_file_url", "File URL must resolve to a public address.", param)
		}
	}
	return parsed, nil
}
