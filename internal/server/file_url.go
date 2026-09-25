package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"

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

// specialUsePrefixes are prefixes that are not globally routable but are not
// covered by net.IP's IsPrivate/IsLoopback/IsLinkLocal helpers. They are
// defined in RFC 5735 (special-use), RFC 6598 (shared address space),
// RFC 2544 (benchmarking), RFC 6890 (special-purpose), RFC 6052 (NAT64),
// RFC 8215 (local-use NAT64), RFC 6666 (discard-only), RFC 3849
// (documentation), RFC 5180 (benchmarking), RFC 7343 (ORCHID), RFC 3056
// (6to4), and RFC 4380 (Teredo). Rejecting them prevents SSRF to internal
// infrastructure that happens to use these ranges.
var specialUsePrefixes = []netip.Prefix{
	// IPv4
	netip.MustParsePrefix("0.0.0.0/8"),       // RFC 5735 "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 shared address space (CGNAT)
	netip.MustParsePrefix("192.0.0.0/24"),    // RFC 5735 IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // RFC 5737 TEST-NET-1
	netip.MustParsePrefix("192.88.99.2/32"),  // IANA 6a44 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),   // RFC 2544 benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // RFC 5737 TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // RFC 5737 TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // RFC 5735 reserved for future use
	// IPv6
	netip.MustParsePrefix("::/96"),          // RFC 4291 IPv4-compatible (deprecated)
	netip.MustParsePrefix("64:ff9b::/96"),   // RFC 6052 well-known NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"), // RFC 8215 local-use NAT64
	netip.MustParsePrefix("100::/64"),       // RFC 6666 discard-only
	netip.MustParsePrefix("100:0:0:1::/64"), // IANA dummy IPv6 prefix
	netip.MustParsePrefix("2001::/23"),      // RFC 6890 IETF protocol assignments (incl. Teredo, benchmarking, ORCHID)
	netip.MustParsePrefix("2001:db8::/32"),  // RFC 3849 documentation
	netip.MustParsePrefix("2002::/16"),      // RFC 3056 6to4
	netip.MustParsePrefix("3fff::/20"),      // RFC 9637 documentation
	netip.MustParsePrefix("5f00::/16"),      // IANA segment routing SIDs
}

// isPublicIP reports whether ip is a globally routable public address.
// Loopback, private, link-local, unspecified, multicast, interface-local
// multicast, and special-use addresses are all rejected.
func isPublicIP(ip net.IP) bool {
	if ip.IsPrivate() ||
		ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() {
		return false
	}
	// IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) are handled by the
	// net.IP helpers above, but the special-use prefixes below are
	// checked on the unmapped form as well.
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	if address.Is4In6() {
		address = address.Unmap()
	}
	for _, prefix := range specialUsePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// downloadPublicHTTPS downloads a file from a public HTTPS URL, following up
// to four redirects and re-validating each one. It returns the content and a
// filename derived from the final URL path.
func (server *Server) downloadPublicHTTPS(ctx context.Context, rawURL, param string) ([]byte, string, error) {
	parsed, err := server.validatePublicHTTPS(ctx, rawURL, param)
	if err != nil {
		return nil, "", err
	}
	client := server.fileDownloadClient
	current := parsed
	redirects := 0
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		upstream, err := client.Do(request)
		if err != nil {
			var urlError *url.Error
			if errors.As(err, &urlError) && strings.Contains(urlError.Err.Error(), "failed to parse Location header") {
				return nil, "", apierror.New(400, "invalid_file_url", "Redirect location is invalid.", param)
			}
			slog.Warn("file URL download failed", "host", current.Hostname(), "error", safeHTTPError(err))
			return nil, "", apierror.New(400, "invalid_file_url", "Unable to download file URL.", param)
		}
		if upstream.StatusCode >= 300 && upstream.StatusCode < 400 {
			location := upstream.Header.Get("Location")
			_ = upstream.Body.Close()
			if redirects >= 4 {
				return nil, "", apierror.New(400, "invalid_file_url", "Too many redirects.", param)
			}
			next, err := current.Parse(location)
			if err != nil {
				return nil, "", apierror.New(400, "invalid_file_url", "Redirect location is invalid.", param)
			}
			current, err = server.validatePublicHTTPS(ctx, next.String(), param)
			if err != nil {
				return nil, "", err
			}
			redirects++
			continue
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			_ = upstream.Body.Close()
			slog.Warn("file URL returned non-success status", "host", current.Hostname(), "status", upstream.StatusCode)
			return nil, "", apierror.New(400, "invalid_file_url", "Unable to download file URL.", param)
		}
		var content []byte
		if server.settings.MaxFileBytes > 0 {
			content, err = io.ReadAll(io.LimitReader(upstream.Body, server.settings.MaxFileBytes+1))
		} else {
			content, err = io.ReadAll(upstream.Body)
		}
		_ = upstream.Body.Close()
		if err != nil {
			slog.Warn("file URL response read failed", "host", current.Hostname(), "error", err)
			return nil, "", err
		}
		return content, filepath.Base(current.Path), nil
	}
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
