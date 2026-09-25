package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/config"
)

// fakeResolver returns a fixed set of addresses for every hostname.
type fakeResolver struct {
	addresses []net.IPAddr
	err       error
}

func (resolver fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return resolver.addresses, resolver.err
}

// switchingResolver returns public addresses on the first lookup and private
// addresses afterwards, simulating a DNS rebinding attack.
type switchingResolver struct {
	mu      sync.Mutex
	lookups int
	public  []net.IPAddr
	private []net.IPAddr
}

func (resolver *switchingResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.lookups++
	if resolver.lookups == 1 {
		return resolver.public, nil
	}
	return resolver.private, nil
}

// recordingDialer records the addresses it was asked to dial.
type recordingDialer struct {
	mu        sync.Mutex
	addresses []string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func (dialer *recordingDialer) DialContext(_ context.Context, network, address string) (net.Conn, error) {
	dialer.mu.Lock()
	dialer.addresses = append(dialer.addresses, address)
	dialer.mu.Unlock()
	return nil, errors.New("dial not allowed in test")
}

func TestIsPublicIP(t *testing.T) {
	for _, test := range []struct {
		name string
		ip   string
		want bool
	}{
		{name: "public_ipv4", ip: "8.8.8.8", want: true},
		{name: "public_ipv6", ip: "2606:4700:4700::1111", want: true},
		{name: "loopback_ipv4", ip: "127.0.0.1", want: false},
		{name: "loopback_ipv6", ip: "::1", want: false},
		{name: "private_ipv4", ip: "10.0.0.1", want: false},
		{name: "private_ipv4_172", ip: "172.16.0.1", want: false},
		{name: "private_ipv4_192", ip: "192.168.1.1", want: false},
		{name: "link_local_ipv4", ip: "169.254.1.1", want: false},
		{name: "link_local_ipv6", ip: "fe80::1", want: false},
		{name: "unspecified_ipv4", ip: "0.0.0.0", want: false},
		{name: "unspecified_ipv6", ip: "::", want: false},
		{name: "multicast_ipv4", ip: "224.0.0.1", want: false},
		{name: "multicast_ipv6", ip: "ff02::1", want: false},
		{name: "shared_address_space", ip: "100.64.0.1", want: false},
		{name: "shared_address_space_upper", ip: "100.127.255.255", want: false},
		{name: "benchmark_network", ip: "198.18.0.1", want: false},
		{name: "benchmark_network_upper", ip: "198.19.255.255", want: false},
		{name: "ietf_protocol_assignments", ip: "192.0.0.1", want: false},
		{name: "test_net_1", ip: "192.0.2.1", want: false},
		{name: "six_a_four_relay_anycast", ip: "192.88.99.2", want: false},
		{name: "test_net_2", ip: "198.51.100.1", want: false},
		{name: "test_net_3", ip: "203.0.113.1", want: false},
		{name: "reserved_240", ip: "240.0.0.1", want: false},
		{name: "broadcast", ip: "255.255.255.255", want: false},
		{name: "ipv4_mapped_shared", ip: "::ffff:100.64.0.1", want: false},
		{name: "ipv4_mapped_benchmark", ip: "::ffff:198.18.0.1", want: false},
		{name: "ipv4_mapped_public", ip: "::ffff:8.8.8.8", want: true},
		{name: "this_network_ipv4", ip: "0.0.0.1", want: false},
		{name: "nat64_well_known", ip: "64:ff9b::a9fe:a9fe", want: false},
		{name: "nat64_well_known_metadata", ip: "64:ff9b::7f00:1", want: false},
		{name: "nat64_local_use", ip: "64:ff9b:1::a9fe:a9fe", want: false},
		{name: "ipv4_compatible", ip: "::8.8.8.8", want: false},
		{name: "discard_only", ip: "100::1", want: false},
		{name: "dummy_ipv6", ip: "100:0:0:1::1", want: false},
		{name: "teredo", ip: "2001::1", want: false},
		{name: "benchmark_ipv6", ip: "2001:2::1", want: false},
		{name: "documentation_ipv6", ip: "2001:db8::1", want: false},
		{name: "orchid", ip: "2001:10::1", want: false},
		{name: "six_to_four", ip: "2002:0808:0808::1", want: false},
		{name: "documentation_ipv6_2", ip: "3fff::1", want: false},
		{name: "segment_routing_sids", ip: "5f00::1", want: false},
		{name: "public_ipv6_after_2001", ip: "2001:4860:4860::8888", want: true},
		{name: "public_ipv6_after_2002", ip: "2400:cb00:2048:1::c629:d7a2", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ip := net.ParseIP(test.ip)
			if ip == nil {
				t.Fatalf("invalid test IP %q", test.ip)
			}
			if got := isPublicIP(ip); got != test.want {
				t.Fatalf("isPublicIP(%s) = %t, want %t", test.ip, got, test.want)
			}
		})
	}
}

func TestValidatePublicHTTPS(t *testing.T) {
	server := &Server{resolver: fakeResolver{addresses: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}}}
	for _, test := range []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{name: "public_https", rawURL: "https://example.com/file.txt", wantErr: false},
		{name: "http_rejected", rawURL: "http://example.com/file.txt", wantErr: true},
		{name: "non_443_port_rejected", rawURL: "https://example.com:8443/file.txt", wantErr: true},
		{name: "userinfo_rejected", rawURL: "https://user:pass@example.com/file.txt", wantErr: true},
		{name: "no_host_rejected", rawURL: "https:///file.txt", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := server.validatePublicHTTPS(context.Background(), test.rawURL, "input[0].file_url")
			if (err != nil) != test.wantErr {
				t.Fatalf("validatePublicHTTPS(%q) error = %v, wantErr %t", test.rawURL, err, test.wantErr)
			}
		})
	}
}

func TestValidatePublicHTTPSRejectsPrivateAddresses(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1", "::1", "fe80::1", "0.0.0.0", "224.0.0.1"} {
		server := &Server{resolver: fakeResolver{addresses: []net.IPAddr{{IP: net.ParseIP(ip)}}}}
		_, err := server.validatePublicHTTPS(context.Background(), "https://example.com/file.txt", "input[0].file_url")
		var gatewayError *apierror.Error
		if !errors.As(err, &gatewayError) || gatewayError.Status != 400 || gatewayError.Code != "invalid_file_url" {
			t.Fatalf("validatePublicHTTPS(%s) error = %v, want invalid_file_url", ip, err)
		}
	}
}

func TestPublicDialerPinsValidatedIP(t *testing.T) {
	resolver := &switchingResolver{
		public:  []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}},
		private: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}},
	}
	recorder := &recordingDialer{}
	dialer := &publicDialer{resolver: resolver, dial: recorder.DialContext}

	// First lookup (validation) returns the public IP; the dial-time lookup
	// returns the private IP. The dialer must reject the private IP and never
	// dial it.
	if _, err := resolver.LookupIPAddr(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}
	_, err := dialer.DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("DialContext() succeeded for a rebinding hostname")
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("DialContext() error = %v, want non-public rejection", err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.addresses) != 0 {
		t.Fatalf("dialed addresses = %v, want none", recorder.addresses)
	}
}

func TestPublicDialerDialsValidatedAddress(t *testing.T) {
	resolver := fakeResolver{addresses: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}}
	recorder := &recordingDialer{}
	dialer := &publicDialer{resolver: resolver, dial: recorder.DialContext}
	_, err := dialer.DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("DialContext() succeeded despite dialer failure")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.addresses) != 1 || recorder.addresses[0] != "8.8.8.8:443" {
		t.Fatalf("dialed addresses = %v, want [8.8.8.8:443]", recorder.addresses)
	}
}

func TestPublicDialerRejectsMixedAddresses(t *testing.T) {
	resolver := fakeResolver{addresses: []net.IPAddr{
		{IP: net.ParseIP("8.8.8.8")},
		{IP: net.ParseIP("10.0.0.1")},
	}}
	recorder := &recordingDialer{}
	dialer := &publicDialer{resolver: resolver, dial: recorder.DialContext}
	_, err := dialer.DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("DialContext() succeeded with a private address in the set")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.addresses) != 0 {
		t.Fatalf("dialed addresses = %v, want none", recorder.addresses)
	}
}

func TestDownloadPublicHTTPSRedirectsRevalidated(t *testing.T) {
	// The first hop resolves to a public IP and returns a redirect to a
	// private address; the second hop must be rejected by validation.
	first := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Location", "https://internal.example.com/secret.txt")
		response.WriteHeader(http.StatusFound)
	}))
	defer first.Close()
	firstURL, _ := url.Parse(first.URL)
	firstHost := firstURL.Hostname()
	firstPort := firstURL.Port()

	// Build a server whose resolver maps the public host to the test server
	// and the redirect host to a private address.
	server := &Server{settings: configForFileURLTest()}
	server.resolver = &hostResolver{
		hosts: map[string][]net.IPAddr{
			firstHost:              {{IP: net.ParseIP("8.8.8.8")}},
			"internal.example.com": {{IP: net.ParseIP("10.0.0.1")}},
		},
	}
	server.fileDialer = &publicDialer{
		resolver: server.resolver,
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// Route the pinned public IP to the local test server.
			host, _, _ := net.SplitHostPort(address)
			if host == "8.8.8.8" {
				return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(firstHost, firstPort))
			}
			return nil, errors.New("unexpected dial")
		},
	}
	server.fileDownloadClient = fileDownloadClientForTest(server.fileDialer)

	_, _, err := server.downloadPublicHTTPS(context.Background(), "https://"+firstHost+"/file.txt", "input[0].file_url")
	var gatewayError *apierror.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != "invalid_file_url" {
		t.Fatalf("downloadPublicHTTPS() error = %v, want invalid_file_url", err)
	}
}

func TestDownloadPublicHTTPSKeepsHostAndTLSName(t *testing.T) {
	// The dialer must receive the original hostname (not the IP) so that the
	// TLS ServerName and HTTP Host header are preserved.
	var dialedHost string
	server := &Server{settings: configForFileURLTest()}
	server.resolver = fakeResolver{addresses: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}}
	server.fileDialer = &publicDialer{
		resolver: server.resolver,
		dial: func(_ context.Context, network, address string) (net.Conn, error) {
			dialedHost, _, _ = net.SplitHostPort(address)
			return nil, errors.New("dial not allowed in test")
		},
	}
	server.fileDownloadClient = fileDownloadClientForTest(server.fileDialer)
	_, _, err := server.downloadPublicHTTPS(context.Background(), "https://example.com/file.txt", "input[0].file_url")
	if err == nil {
		t.Fatal("downloadPublicHTTPS() succeeded despite dial failure")
	}
	if dialedHost != "8.8.8.8" {
		t.Fatalf("dialed host = %q, want the validated IP 8.8.8.8", dialedHost)
	}
}

// redirectChainServer builds a server whose resolver maps every host in the
// chain to the local test server, and whose dialer routes the pinned public
// IP to the test server. The chain server returns a redirect to the next host
// for every request until the final host serves the file content.
func redirectChainServer(t *testing.T, hosts []string, finalContent string) (*Server, *httptest.Server) {
	t.Helper()
	chain := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		host := request.Host
		index := -1
		for i, candidate := range hosts {
			if candidate == host {
				index = i
				break
			}
		}
		if index < 0 || index == len(hosts)-1 {
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte(finalContent))
			return
		}
		response.Header().Set("Location", "https://"+hosts[index+1]+"/file.txt")
		response.WriteHeader(http.StatusFound)
	}))
	chainURL, _ := url.Parse(chain.URL)
	chainHost := chainURL.Hostname()
	chainPort := chainURL.Port()

	addresses := make(map[string][]net.IPAddr, len(hosts))
	for _, host := range hosts {
		addresses[host] = []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}
	}
	server := &Server{settings: configForFileURLTest()}
	server.resolver = &hostResolver{hosts: addresses}
	server.fileDialer = &publicDialer{
		resolver: server.resolver,
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(address)
			if host == "8.8.8.8" {
				return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(chainHost, chainPort))
			}
			return nil, errors.New("unexpected dial")
		},
	}
	server.fileDownloadClient = fileDownloadClientForTest(server.fileDialer)
	// Trust the test server's self-signed certificate so the HTTPS client
	// accepts the connection.
	server.fileDownloadClient.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return server, chain
}

func TestDownloadPublicHTTPSAllowsFourRedirects(t *testing.T) {
	hosts := []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com"}
	server, chain := redirectChainServer(t, hosts, "final content")
	defer chain.Close()

	content, filename, err := server.downloadPublicHTTPS(context.Background(), "https://"+hosts[0]+"/file.txt", "input[0].file_url")
	if err != nil {
		t.Fatalf("downloadPublicHTTPS() error = %v", err)
	}
	if string(content) != "final content" {
		t.Fatalf("content = %q, want %q", content, "final content")
	}
	if filename != "file.txt" {
		t.Fatalf("filename = %q, want file.txt", filename)
	}
}

func TestDownloadPublicHTTPSRejectsFifthRedirect(t *testing.T) {
	hosts := []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com", "f.example.com"}
	server, chain := redirectChainServer(t, hosts, "final content")
	defer chain.Close()

	_, _, err := server.downloadPublicHTTPS(context.Background(), "https://"+hosts[0]+"/file.txt", "input[0].file_url")
	var gatewayError *apierror.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != "invalid_file_url" || !strings.Contains(gatewayError.Message, "Too many redirects") {
		t.Fatalf("downloadPublicHTTPS() error = %v, want Too many redirects", err)
	}
}

func TestDownloadPublicHTTPSRejectsMalformedRedirectLocation(t *testing.T) {
	server := &Server{
		settings: configForFileURLTest(),
		resolver: fakeResolver{addresses: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}},
	}
	server.fileDownloadClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://%"}},
			Body:       http.NoBody,
			Request:    request,
		}, nil
	})}
	server.fileDownloadClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

	_, _, err := server.downloadPublicHTTPS(context.Background(), "https://example.com/file.txt", "input[0].file_url")
	var gatewayError *apierror.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != "invalid_file_url" || !strings.Contains(gatewayError.Message, "Redirect location is invalid") {
		t.Fatalf("downloadPublicHTTPS() error = %v, want invalid redirect location", err)
	}
}

func TestDownloadPublicHTTPSUnlimitedSize(t *testing.T) {
	// With MaxFileBytes = 0 the download is not size-limited, so content
	// larger than the default test limit is accepted.
	hosts := []string{"a.example.com"}
	content := strings.Repeat("x", 4096)
	server, chain := redirectChainServer(t, hosts, content)
	defer chain.Close()
	server.settings.MaxFileBytes = 0

	downloaded, filename, err := server.downloadPublicHTTPS(context.Background(), "https://"+hosts[0]+"/file.txt", "input[0].file_url")
	if err != nil {
		t.Fatalf("downloadPublicHTTPS() error = %v", err)
	}
	if string(downloaded) != content {
		t.Fatalf("content length = %d, want %d", len(downloaded), len(content))
	}
	if filename != "file.txt" {
		t.Fatalf("filename = %q, want file.txt", filename)
	}
}

// hostResolver maps hostnames to fixed addresses.
type hostResolver struct {
	hosts map[string][]net.IPAddr
}

func (resolver *hostResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addresses, ok := resolver.hosts[host]
	if !ok {
		return nil, errors.New("unknown host")
	}
	return addresses, nil
}

// fileDownloadClientForTest builds the shared file download client the same
// way NewHandler does, so tests can exercise downloadPublicHTTPS without
// constructing the full server.
func fileDownloadClientForTest(dialer *publicDialer) *http.Client {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
		},
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func configForFileURLTest() config.Config {
	return config.Config{MaxFileBytes: 1024, RequestTimeout: 5 * time.Second}
}
