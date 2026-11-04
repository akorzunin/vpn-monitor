package monitor

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Result
		want string
	}{
		{"healthy", Result{Controls: []Sample{{OK: true}}, Samples: []Sample{{OK: true}}}, "healthy"},
		{"controls", Result{Controls: []Sample{{OK: false}}, Samples: []Sample{{OK: true}}}, "inconclusive"},
		{"startup and controls", Result{Controls: []Sample{{OK: false}}}, "inconclusive"},
		{"partial HTTP", Result{Samples: []Sample{{OK: false}}, FailurePercent: 50}, "degraded"},
		{"all HTTP", Result{Samples: []Sample{{OK: false}}, FailurePercent: 100}, "down"},
		{"no samples", Result{}, "error"},
		{"UDP failed", Result{Samples: []Sample{{OK: true}}, UDP: UDPResult{State: "failed"}}, "degraded"},
		{"speed failed", Result{Samples: []Sample{{OK: true}}, Speed: SpeedResult{State: "error"}}, "degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.r); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
func TestLatencyIncludesP95(t *testing.T) {
	mean, p95 := latencies([]float64{5, 1, 3, 2, 4})
	if mean != 3 || p95 != 5 {
		t.Fatal(mean, p95)
	}
}
func TestHTTPExpectedStatusRedirectAndTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/ok", 302)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	sample, e := getRequest(context.Background(), client, Target{Name: "x", URL: server.URL, Status: 204})
	if e != nil || !sample.OK || sample.HTTPStatus != 204 {
		t.Fatal(sample, e)
	}
	sample, _ = getRequest(context.Background(), client, Target{Name: "x", URL: server.URL + "/redirect", Status: 204})
	if sample.OK || sample.HTTPStatus != 302 {
		t.Fatal("redirect followed", sample)
	}
	// Production clients must not trust the self-signed test certificate.
	sample, e = getRequest(context.Background(), httpClient("", time.Second), Target{Name: "x", URL: server.URL, Status: 204})
	if e == nil || sample.OK || strings.Contains(sample.Error, server.URL) {
		t.Fatal("TLS verification or redaction failed", sample, e)
	}
}
func TestSpeedBoundAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		encoding string
		body     string
		want     string
		bytes    int64
	}{
		{"bounded", 200, "", strings.Repeat("x", 4096), "ok", 1024},
		{"empty", 200, "", "", "error", 0},
		{"bad status", 403, "", "denied", "error", 0},
		{"compressed", 200, "gzip", "compressed", "error", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			r := speedTest(context.Background(), server.Client(), SpeedConfig{URL: server.URL, MaxBytes: 1024, Timeout: time.Second})
			if r.State != tc.want || r.Bytes != tc.bytes {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestHTTPTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client := server.Client()
	client.Timeout = 20 * time.Millisecond
	sample, e := getRequest(context.Background(), client, Target{Name: "slow", URL: server.URL, Status: 200})
	if e == nil || sample.OK {
		t.Fatal("timeout passed", sample)
	}
}
func TestDirectClientIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client := httpClient("", time.Second)
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("environment proxy enabled")
	}
}
func TestRealSingBoxIsolatedTLSRequest(t *testing.T) {
	binary, e := exec.LookPath("sing-box")
	if e != nil {
		t.Skip("sing-box not installed")
	}
	// A local SOCKS upstream exercises the real client without contacting a VPN or changing OS routes.
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				var greeting [3]byte
				if _, e := io.ReadFull(c, greeting[:]); e != nil {
					return
				}
				_, _ = c.Write([]byte{5, 0})
				var header [3]byte
				if _, e := io.ReadFull(c, header[:]); e != nil {
					return
				}
				address, e := readSOCKSAddress(c)
				if e != nil {
					return
				}
				up, e := net.DialTimeout("tcp", address, time.Second)
				if e != nil {
					return
				}
				defer up.Close()
				_, _ = c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}(c)
		}
	}()
	upstream := listener.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address, stop, e := startClient(ctx, binary, Route{Outbound: map[string]any{"type": "socks", "server": "localhost", "server_port": upstream.Port, "version": "5"}})
	if e != nil {
		t.Fatal(e)
	}
	defer stop()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	client := httpClient(address, time.Second)
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS12}
	result, e := getRequest(ctx, client, Target{Name: "local", URL: server.URL, Status: 204})
	if e != nil || !result.OK {
		t.Fatal(result, e)
	}
}
func TestClientMissingExecutableAndCleanup(t *testing.T) {
	_, _, e := startClient(context.Background(), "/does/not/exist", Route{Outbound: map[string]any{"type": "socks"}})
	if e == nil {
		t.Fatal("missing executable passed")
	}
	// Ensure a failing core doesn't expose its output (which can contain secrets).
	binary := filepath.Join(t.TempDir(), "failing-core")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho private-credential >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, _, e = startClient(context.Background(), binary, Route{Outbound: map[string]any{"type": "socks"}})
	if e == nil || strings.Contains(e.Error(), "private-credential") {
		t.Fatal("failing core accepted or credentials exposed", e)
	}
}
