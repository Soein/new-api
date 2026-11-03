package service

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticSSRFResolver map[string][]net.IPAddr

func (r staticSSRFResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if ips, ok := r[host]; ok {
		return ips, nil
	}
	return nil, fmt.Errorf("unexpected lookup for %s", host)
}

func staticProtection(protection *common.SSRFProtection) func() (*common.SSRFProtection, bool, error) {
	return func() (*common.SSRFProtection, bool, error) {
		return protection, true, nil
	}
}

func testConn(t *testing.T) net.Conn {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	return clientConn
}

func configureSSRFTestFetchSetting(t *testing.T) {
	t.Helper()
	fetchSetting := system_setting.GetFetchSetting()
	original := *fetchSetting
	t.Cleanup(func() {
		*fetchSetting = original
	})

	fetchSetting.EnableSSRFProtection = true
	fetchSetting.AllowPrivateIp = false
	fetchSetting.DomainFilterMode = false
	fetchSetting.IpFilterMode = false
	fetchSetting.DomainList = nil
	fetchSetting.IpList = nil
	fetchSetting.AllowedPorts = []string{"80", "443"}
	fetchSetting.ApplyIPFilterForDomain = true
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	parsedURL, err := url.Parse(rawURL)
	require.NoError(t, err)
	return parsedURL
}

func TestProtectedFetchDialerRejectsPrivateReboundAddress(t *testing.T) {
	dialer := &protectedFetchDialer{
		resolver: staticSSRFResolver{
			"safe.example": {{IP: net.ParseIP("127.0.0.1")}},
		},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			t.Fatalf("dialContext should not be called for blocked address %s", address)
			return nil, nil
		},
		getProtection: staticProtection(&common.SSRFProtection{
			AllowPrivateIp:         false,
			DomainFilterMode:       false,
			IpFilterMode:           false,
			ApplyIPFilterForDomain: true,
		}),
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", "safe.example:80")

	require.Error(t, err)
	require.Nil(t, conn)
	require.Contains(t, err.Error(), "private IP address not allowed")
}

func TestProtectedFetchDialerRejectsMixedResolvedIPs(t *testing.T) {
	var dialed []string
	dialer := &protectedFetchDialer{
		resolver: staticSSRFResolver{
			"safe.example": {
				{IP: net.ParseIP("10.0.0.1")},
				{IP: net.ParseIP("8.8.8.8")},
			},
		},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return testConn(t), nil
		},
		getProtection: staticProtection(&common.SSRFProtection{
			AllowPrivateIp:         false,
			DomainFilterMode:       false,
			IpFilterMode:           false,
			ApplyIPFilterForDomain: true,
		}),
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", "safe.example:443")
	require.Error(t, err)
	require.Nil(t, conn)

	require.Empty(t, dialed)
	require.Contains(t, err.Error(), "private IP address not allowed")
}

func TestProtectedFetchDialerDialsWhenAllResolvedIPsAllowed(t *testing.T) {
	var dialed []string
	dialer := &protectedFetchDialer{
		resolver: staticSSRFResolver{
			"safe.example": {
				{IP: net.ParseIP("8.8.8.8")},
				{IP: net.ParseIP("1.1.1.1")},
			},
		},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return testConn(t), nil
		},
		getProtection: staticProtection(&common.SSRFProtection{
			AllowPrivateIp:         false,
			DomainFilterMode:       false,
			IpFilterMode:           false,
			ApplyIPFilterForDomain: true,
		}),
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", "safe.example:443")
	require.NoError(t, err)
	require.NotNil(t, conn)

	require.Equal(t, []string{"8.8.8.8:443"}, dialed)
}

func TestProtectedFetchDialerAllowsPrivateIPWhenWhitelisted(t *testing.T) {
	var dialed []string
	dialer := &protectedFetchDialer{
		resolver: staticSSRFResolver{
			"internal.example": {{IP: net.ParseIP("10.1.2.3")}},
		},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return testConn(t), nil
		},
		getProtection: staticProtection(&common.SSRFProtection{
			AllowPrivateIp:         true,
			DomainFilterMode:       false,
			IpFilterMode:           true,
			IpList:                 []string{"10.0.0.0/8"},
			ApplyIPFilterForDomain: true,
		}),
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", "internal.example:80")
	require.NoError(t, err)
	require.NotNil(t, conn)

	require.Equal(t, []string{"10.1.2.3:80"}, dialed)
}

func TestProtectedFetchDialerSkipsResolvedIPCheckWhenDisabled(t *testing.T) {
	var dialed []string
	dialer := &protectedFetchDialer{
		resolver: staticSSRFResolver{},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return testConn(t), nil
		},
		getProtection: staticProtection(&common.SSRFProtection{
			AllowPrivateIp:         false,
			DomainFilterMode:       false,
			IpFilterMode:           false,
			ApplyIPFilterForDomain: false,
		}),
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", "safe.example:80")
	require.NoError(t, err)
	require.NotNil(t, conn)

	require.Equal(t, []string{"safe.example:80"}, dialed)
}

func TestProtectedFetchRejectsScopedIPv6(t *testing.T) {
	for _, applyIPFilter := range []bool{false, true} {
		for _, allowPrivateIP := range []bool{false, true} {
			t.Run(fmt.Sprintf("domain_ip_filter=%t/private_ip=%t", applyIPFilter, allowPrivateIP), func(t *testing.T) {
				configureSSRFTestFetchSetting(t)
				setting := system_setting.GetFetchSetting()
				setting.ApplyIPFilterForDomain = applyIPFilter
				setting.AllowPrivateIp = allowPrivateIP
				setting.AllowedPorts = []string{"80", "443", "8080", "8443"}
				for _, target := range []string{
					"https://[::1%25lo0]/",
					"https://[::1%25lo0]:443/",
					"http://[::1%251]/",
					"http://[::1%251]:8080/",
					"https://[fe80::1%25eth0]/",
					"https://[fe80::1%25eth0]:8443/",
					"http://[fe80::1%251]/",
					"http://[fe80::1%251]:80/",
					"https://[2606:4700:4700::1111%25eth0]/",
				} {
					t.Run(target, func(t *testing.T) {
						assert.ErrorContains(t, ValidateSSRFProtectedFetchURL(target), "scoped IPv6 addresses are not allowed")
						dialed := make(chan string, 1)
						transport, err := newSSRFProtectedRedirectTransport(&http.Transport{}, "", &protectedFetchDialer{
							resolver:      staticSSRFResolver{},
							getProtection: currentFetchProtection,
							dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
								dialed <- address
								return nil, errors.New("unexpected scoped IPv6 socket")
							},
						})
						require.NoError(t, err)
						defer closeIdleConnections(transport)
						request, err := http.NewRequest(http.MethodGet, target, nil)
						require.NoError(t, err)
						response, err := transport.RoundTrip(request)
						assert.ErrorContains(t, err, "scoped IPv6 addresses are not allowed")
						assert.Nil(t, response)
						assert.Empty(t, dialed, "scoped literals must be rejected before opening a socket")
					})
				}
			})
		}
	}
}

func TestProtectedFetchUnscopedTargetsRetainPolicy(t *testing.T) {
	configureSSRFTestFetchSetting(t)
	system_setting.GetFetchSetting().ApplyIPFilterForDomain = false
	system_setting.GetFetchSetting().AllowedPorts = []string{"80", "443", "8080", "8443"}
	for _, tc := range []struct {
		url         string
		destination string
		blocked     bool
	}{
		{url: "https://[::1]/", blocked: true},
		{url: "http://[fe80::1]:8080/", blocked: true},
		{url: "https://[2606:4700:4700::1111]/", destination: "[2606:4700:4700::1111]:443"},
		{url: "http://[2606:4700:4700::1111]:8080/", destination: "[2606:4700:4700::1111]:8080"},
		{url: "http://93.184.216.34/", destination: "93.184.216.34:80"},
		{url: "https://93.184.216.34:8443/", destination: "93.184.216.34:8443"},
		{url: "http://public.example/", destination: "public.example:80"},
		{url: "https://public.example:8443/", destination: "public.example:8443"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			urlErr := ValidateSSRFProtectedFetchURL(tc.url)
			if tc.blocked {
				assert.ErrorContains(t, urlErr, "private IP address not allowed")
			} else {
				require.NoError(t, urlErr)
			}
			dialed := make(chan string, 1)
			transport, err := newSSRFProtectedRedirectTransport(&http.Transport{}, "", &protectedFetchDialer{
				resolver:      staticSSRFResolver{},
				getProtection: currentFetchProtection,
				dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					dialed <- address
					return nil, errors.New("checked destination")
				},
			})
			require.NoError(t, err)
			defer closeIdleConnections(transport)
			request, err := http.NewRequest(http.MethodGet, tc.url, nil)
			require.NoError(t, err)
			response, err := transport.RoundTrip(request)
			assert.Nil(t, response)
			if tc.blocked {
				assert.ErrorContains(t, err, "private IP address not allowed")
				assert.Empty(t, dialed)
				return
			}
			require.ErrorContains(t, err, "checked destination")
			require.Len(t, dialed, 1)
			assert.Equal(t, tc.destination, <-dialed)
		})
	}
}

func TestGetSSRFProtectedHTTPClientFallsBackToDefaultClientWhenProtectionDisabled(t *testing.T) {
	fetchSetting := system_setting.GetFetchSetting()
	originalFetchSetting := *fetchSetting
	originalHTTPClient := httpClient
	originalProtectedClient := ssrfProtectedHTTPClient
	t.Cleanup(func() {
		*fetchSetting = originalFetchSetting
		httpClient = originalHTTPClient
		ssrfProtectedHTTPClient = originalProtectedClient
	})

	fetchSetting.EnableSSRFProtection = false
	expected := &http.Client{}
	httpClient = expected
	ssrfProtectedHTTPClient = &http.Client{}

	require.Same(t, expected, GetSSRFProtectedHTTPClient())
}

func TestProtectedFetchRoundTripperUsesConfiguredProxy(t *testing.T) {
	configureSSRFTestFetchSetting(t)
	proxyURL := mustParseURL(t, "http://127.0.0.1:3128")
	var dialed []string
	client := newProtectedFetchHTTPClientWithProxy(
		staticSSRFResolver{},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("stop after proxy dial")
		},
		staticProtection(&common.SSRFProtection{
			AllowPrivateIp:         false,
			DomainFilterMode:       false,
			IpFilterMode:           false,
			ApplyIPFilterForDomain: true,
		}),
		func(req *http.Request) (*url.URL, error) {
			return proxyURL, nil
		},
	)
	req, err := http.NewRequest(http.MethodGet, "http://93.184.216.34/resource", nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.Error(t, err)
	require.Nil(t, resp)
	require.Equal(t, []string{"127.0.0.1:3128"}, dialed)
}

func TestProtectedFetchRoundTripperRejectsPrivateTargetBeforeProxy(t *testing.T) {
	configureSSRFTestFetchSetting(t)
	proxyURL := mustParseURL(t, "http://127.0.0.1:3128")
	var dialed []string
	client := newProtectedFetchHTTPClientWithProxy(
		staticSSRFResolver{},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("proxy should not be dialed")
		},
		staticProtection(&common.SSRFProtection{
			AllowPrivateIp:         false,
			DomainFilterMode:       false,
			IpFilterMode:           false,
			ApplyIPFilterForDomain: true,
		}),
		func(req *http.Request) (*url.URL, error) {
			return proxyURL, nil
		},
	)
	req, err := http.NewRequest(http.MethodGet, "http://localhost/resource", nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.Error(t, err)
	require.Nil(t, resp)
	require.Contains(t, err.Error(), "private IP address not allowed")
	require.Empty(t, dialed)
}

func TestProtectedFetchRoundTripperNoProxyUsesProtectedDialer(t *testing.T) {
	configureSSRFTestFetchSetting(t)
	var dialed []string
	client := newProtectedFetchHTTPClientWithProxy(
		staticSSRFResolver{},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("unexpected direct dial")
		},
		staticProtection(&common.SSRFProtection{
			AllowPrivateIp:         false,
			DomainFilterMode:       false,
			IpFilterMode:           false,
			ApplyIPFilterForDomain: true,
		}),
		func(req *http.Request) (*url.URL, error) {
			return nil, nil
		},
	)
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/resource", nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.Error(t, err)
	require.Nil(t, resp)
	require.Contains(t, err.Error(), "private IP address not allowed")
	require.Empty(t, dialed)
}

func TestProtectedFetchRoundTripperReusesTransportPerProxy(t *testing.T) {
	client := newProtectedFetchHTTPClientWithDialer(nil, nil, nil)
	roundTripper, ok := client.Transport.(*ssrfProtectedRoundTripper)
	require.True(t, ok)

	direct := roundTripper.transportFor(nil)
	directAgain := roundTripper.transportFor(nil)
	proxied := roundTripper.transportFor(mustParseURL(t, "http://127.0.0.1:3128"))

	require.Same(t, direct, directAgain)
	require.NotSame(t, direct, proxied)
	require.True(t, direct.ForceAttemptHTTP2)
	require.False(t, direct.DisableKeepAlives)
}

type changingSSRFResolver struct {
	answers [][]net.IPAddr
	calls   int
}

func (r *changingSSRFResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	answer := r.answers[min(r.calls, len(r.answers)-1)]
	r.calls++
	return answer, nil
}

func TestPublicRedirectTransportPinsDNSAndRechecksLaterConnections(t *testing.T) {
	resolver := &changingSSRFResolver{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("93.184.216.34")}}, {{IP: net.ParseIP("127.0.0.1")}},
	}}
	var dialed []string
	dialer := &protectedFetchDialer{
		resolver:      resolver,
		getProtection: staticProtection(&common.SSRFProtection{ApplyIPFilterForDomain: true}),
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				request, err := http.ReadRequest(bufio.NewReader(server))
				if err != nil {
					return
				}
				_ = request.Body.Close()
				_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			}()
			return client, nil
		},
	}
	transport, err := newSSRFProtectedRedirectTransport(&http.Transport{}, "", dialer)
	require.NoError(t, err)
	defer closeIdleConnections(transport)
	client := &http.Client{Transport: transport}
	response, err := client.Get("http://public.example/result")
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, "ok", string(body))
	assert.Equal(t, []string{"93.184.216.34:80"}, dialed, "the dialer receives an IP, never a hostname for a second lookup")

	response, err = client.Get("http://public.example/next")
	require.ErrorContains(t, err, "private IP address not allowed")
	assert.Nil(t, response)
	assert.Equal(t, 2, resolver.calls)
	assert.Len(t, dialed, 1, "the rebound address never reaches the socket")
}

func TestPublicRedirectTransportRejectsProxies(t *testing.T) {
	for _, proxy := range []string{"http://proxy.example:3128", "socks5://proxy.example:1080", "socks5h://proxy.example:1080"} {
		_, err := NewSSRFProtectedRedirectTransport(&http.Transport{}, proxy)
		require.ErrorContains(t, err, "channel proxy")
	}
	transport, err := NewSSRFProtectedRedirectTransport(&http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		return mustParseURL(t, "http://proxy.example:3128"), nil
	}}, "")
	require.NoError(t, err)
	defer closeIdleConnections(transport)
	response, err := (&http.Client{Transport: transport}).Get("http://93.184.216.34/result")
	require.ErrorContains(t, err, "environment proxy")
	assert.Nil(t, response)
}

func TestPublicRedirectTransportVerifiesTLSAndDropsClientIdentity(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%d", len(r.TLS.PeerCertificates))
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	server.StartTLS()
	defer server.Close()
	base := server.Client().Transport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	base.TLSClientConfig.Certificates = []tls.Certificate{server.TLS.Certificates[0]}
	base.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &server.TLS.Certificates[0], nil }
	base.TLSClientConfig.ClientSessionCache = tls.NewLRUClientSessionCache(1)
	warmTransport := base.Clone()
	defer warmTransport.CloseIdleConnections()
	warm, err := (&http.Client{Transport: warmTransport}).Get(server.URL)
	require.NoError(t, err)
	warmBody, err := io.ReadAll(warm.Body)
	require.NoError(t, err)
	require.NoError(t, warm.Body.Close())
	assert.Equal(t, "1", string(warmBody), "the channel transport holds an authenticated TLS session")
	base.DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
		t.Error("the channel TLS dialer must not bypass protected dialing")
		return nil, errors.New("unprotected TLS dialer")
	}
	dialer := &net.Dialer{}
	protectedDialer := &protectedFetchDialer{resolver: net.DefaultResolver, dialContext: dialer.DialContext, getProtection: staticProtection(&common.SSRFProtection{AllowPrivateIp: true})}
	transport, err := newSSRFProtectedRedirectTransport(base, "", protectedDialer)
	require.NoError(t, err)
	defer closeIdleConnections(transport)
	response, err := (&http.Client{Transport: transport}).Get(server.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, "0", string(body), "an untrusted destination must not receive the channel's client certificate")

	base.TLSClientConfig.InsecureSkipVerify = true
	base.TLSClientConfig.RootCAs = nil
	base.TLSClientConfig.ServerName = "channel.example"
	untrusted, err := newSSRFProtectedRedirectTransport(base, "", protectedDialer)
	require.NoError(t, err)
	defer closeIdleConnections(untrusted)
	response, err = (&http.Client{Transport: untrusted}).Get(server.URL)
	require.ErrorContains(t, err, "certificate")
	assert.Nil(t, response)
	assert.True(t, base.TLSClientConfig.InsecureSkipVerify, "the shared channel transport is unchanged")
}

func TestPublicRedirectTransportProtectsShardedConnections(t *testing.T) {
	base := newShardedRoundTripper(HTTPTransportPolicy{Shards: 2}, func() *http.Transport { return &http.Transport{} })
	var dialed []string
	transport, err := newSSRFProtectedRedirectTransport(base, "", &protectedFetchDialer{
		resolver:      staticSSRFResolver{"public.example": {{IP: net.ParseIP("93.184.216.34")}}},
		getProtection: staticProtection(&common.SSRFProtection{ApplyIPFilterForDomain: true}),
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("checked public socket")
		},
	})
	require.NoError(t, err)
	defer closeIdleConnections(transport)
	for range 2 {
		response, err := (&http.Client{Transport: transport}).Get("http://public.example/result")
		require.ErrorContains(t, err, "checked public socket")
		assert.Nil(t, response)
	}
	assert.Equal(t, []string{"93.184.216.34:80", "93.184.216.34:80"}, dialed)
}
