package monitor

import (
	"context"
	"encoding/pem"
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

func TestFullRunLocal(t *testing.T) {
	if os.Getenv("VPN_MONITOR_TEST_CHILD") == "1" {
		base := os.Getenv("VPN_MONITOR_TEST_HTTPS")
		cfg := Config{SingBox: os.Getenv("VPN_MONITOR_TEST_CORE"), Timeout: time.Second, Samples: 2,
			Controls: []Target{{Name: "a", URL: base + "/control-a", Status: 200}, {Name: "b", URL: base + "/control-b", Status: 200}},
			Targets:  []Target{{Name: "target", URL: base + "/target", Status: 204}},
			Speed:    SpeedConfig{URL: base + "/binary", MaxBytes: 1024, Timeout: time.Second}}
		result := Run(context.Background(), cfg, Route{ID: "local/test", Outbound: map[string]any{"type": "direct"}})
		if result.State != "healthy" || len(result.Samples) != 2 || result.Speed.Bytes != 1024 || result.Speed.State != "ok" || result.UDP.State != "unsupported" || result.Finished.IsZero() {
			t.Fatalf("full run: %+v", result)
		}
		return
	}
	binary, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box not installed")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/target":
			w.WriteHeader(204)
		case "/binary":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, strings.Repeat("x", 4096))
		default:
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	certPath := filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFullRunLocal$")
	cmd.Env = append(os.Environ(), "VPN_MONITOR_TEST_CHILD=1", "VPN_MONITOR_TEST_HTTPS="+server.URL, "VPN_MONITOR_TEST_CORE="+binary, "SSL_CERT_FILE="+certPath, "SSL_CERT_DIR="+t.TempDir())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("local full run: %v\n%s", err, output)
	}
}

func TestRealSingBoxUDPAssociate(t *testing.T) {
	binary, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box not installed")
	}
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.LocalAddr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token := strings.Repeat("local-test-secret", 2)
	done := make(chan error, 1)
	go func() { done <- Echo(ctx, endpoint, token) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	// Direct exists only in this localhost transport integration test; public YAML forbids it.
	address, stop, err := startClient(ctx, binary, Route{Outbound: map[string]any{"type": "direct"}})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	r := udpTest(ctx, address, UDPConfig{Endpoint: endpoint, Token: token, Samples: 3, Timeout: time.Second})
	if r.State != "ok" || r.Sent != 3 || r.Received != 3 || r.LossPercent != 0 {
		t.Fatalf("real core UDP: %+v", r)
	}
}
