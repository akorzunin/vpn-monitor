package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// One isolated client per run. No TUN, selectors, published listeners or direct fallback.
func startClient(ctx context.Context, binary string, route Route) (string, func(), error) {
	dir, err := os.MkdirTemp("", "vpn-monitor-")
	if err != nil {
		return "", nil, errors.New("cannot create private client directory")
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	// Concrete-port handoff has a short bind race; prefer core-reported ephemeral ports if upstream adds them.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cleanup()
		return "", nil, errors.New("cannot allocate client port")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	outbound := map[string]any{}
	for k, v := range route.Outbound {
		outbound[k] = v
	}
	outbound["tag"] = "probe"
	config := map[string]any{
		"log":       map[string]any{"disabled": true},
		"inbounds":  []any{map[string]any{"type": "mixed", "tag": "local", "listen": "127.0.0.1", "listen_port": port}},
		"outbounds": []any{outbound}, "route": map[string]any{"final": "probe"},
	}
	b, err := json.Marshal(config)
	if err != nil {
		cleanup()
		return "", nil, errors.New("cannot encode client configuration")
	}
	path := filepath.Join(dir, "client.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		cleanup()
		return "", nil, errors.New("cannot write client configuration")
	}
	cmd := exec.CommandContext(ctx, binary, "run", "-c", path)
	// Never log core stderr: upstream errors can contain credentials and private endpoints.
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch strings.ToLower(key) {
		case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
			continue
		}
		cmd.Env = append(cmd.Env, item)
	}
	cmd.Env = append(cmd.Env, "NO_COLOR=1")
	if err = cmd.Start(); err != nil {
		cleanup()
		return "", nil, errors.New("cannot start sing-box")
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stop := func() { _ = cmd.Process.Kill(); <-exited; cleanup() }
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-exited:
			cleanup()
			return "", nil, errors.New("sing-box exited before readiness; validate private outbound with sing-box check")
		case <-ctx.Done():
			stop()
			return "", nil, ctx.Err()
		case <-timer.C:
			stop()
			return "", nil, errors.New("sing-box readiness timed out")
		case <-time.After(50 * time.Millisecond):
			conn, e := net.DialTimeout("tcp", address, 100*time.Millisecond)
			if e == nil {
				_ = conn.Close()
				return address, stop, nil
			}
		}
	}
}
func httpClient(address string, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{Proxy: nil, DialContext: dialer.DialContext, DisableKeepAlives: true, DisableCompression: true, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout}
	if address != "" {
		transport.Proxy = http.ProxyURL(&url.URL{Scheme: "socks5h", Host: address})
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}
func getRequest(ctx context.Context, client *http.Client, target Target) (Sample, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", target.URL, nil)
	if err != nil {
		return Sample{}, err
	}
	resp, err := client.Do(req)
	sample := Sample{Target: target.Name, LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
	if err != nil {
		sample.Error = "request failed (DNS, connection, TLS or timeout)"
		return sample, err
	}
	defer resp.Body.Close()
	sample.HTTPStatus = resp.StatusCode
	sample.OK = resp.StatusCode == target.Status
	if !sample.OK {
		sample.Error = fmt.Sprintf("unexpected HTTP status %d", resp.StatusCode)
	}
	return sample, nil
}
