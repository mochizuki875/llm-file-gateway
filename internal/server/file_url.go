package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
)

// ipResolver resolves hostnames to IP addresses. It is an interface so that
// security tests can inject a fake resolver without depending on external
// network access.
type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// publicDialer resolves a hostname, rejects any non-public address, and dials
// the validated address directly. Because the connection is pinned to the IP
// that was validated, a DNS response change between validation and connection
// cannot redirect the request to a private network (DNS rebinding / TOCTOU).
type publicDialer struct {
	resolver ipResolver
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
}

// DialContext implements the net.Dialer-style dial function used by
// http.Transport. It resolves the hostname, validates every returned address,
// and dials the validated addresses in order. The HTTP transport still uses
// the original hostname for the TLS ServerName and the Host header.
func (dialer *publicDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid dial address: %w", err)
	}
	addresses, err := dialer.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, resolved := range addresses {
		if !isPublicIP(resolved.IP) {
			return nil, fmt.Errorf("resolve %s: non-public address %s", host, resolved.IP)
		}
	}
	var lastErr error
	for _, resolved := range addresses {
		conn, err := dialer.dial(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// isPublicIP reports whether ip is a globally routable public address.
// Loopback, private, link-local, unspecified, multicast, and interface-local
// multicast addresses are all rejected.
func isPublicIP(ip net.IP) bool {
	return !ip.IsPrivate() &&
		!ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsInterfaceLocalMulticast() &&
		!ip.IsUnspecified() &&
		!ip.IsMulticast()
}

// fileDownloadClient builds the HTTP client used for file URL downloads. Its
// transport pins every connection to the IP validated by the public dialer,
// and redirects are not followed automatically so each hop can be re-validated.
func (server *Server) fileDownloadClient() *http.Client {
	client := &http.Client{
		Timeout: server.settings.RequestTimeout,
		Transport: &http.Transport{
			DialContext: server.fileDialer.DialContext,
		},
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

// downloadPublicHTTPS downloads a file from a public HTTPS URL, following up
// to four redirects and re-validating each one. It returns the content and a
// filename derived from the final URL path.
func (server *Server) downloadPublicHTTPS(ctx context.Context, rawURL, param string) ([]byte, string, error) {
	parsed, err := server.validatePublicHTTPS(ctx, rawURL, param)
	if err != nil {
		return nil, "", err
	}
	client := server.fileDownloadClient()
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
// SSRF to internal networks. The authoritative validation happens again inside
// the public dialer at connection time.
func (server *Server) validatePublicHTTPS(ctx context.Context, rawURL, param string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "443") {
		return nil, apierror.New(400, "invalid_file_url", "Only public HTTPS URLs on port 443 are allowed.", param)
	}
	addresses, err := server.resolver.LookupIPAddr(ctx, parsed.Hostname())
	if err != nil || len(addresses) == 0 {
		slog.Warn("file URL hostname resolution failed", "host", parsed.Hostname(), "error", err)
		return nil, apierror.New(400, "invalid_file_url", "File URL hostname cannot be resolved.", param)
	}
	for _, address := range addresses {
		if !isPublicIP(address.IP) {
			return nil, apierror.New(400, "invalid_file_url", "File URL must resolve to a public address.", param)
		}
	}
	return parsed, nil
}
