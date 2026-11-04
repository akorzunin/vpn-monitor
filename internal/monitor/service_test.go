package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testService(t *testing.T) *Service {
	t.Helper()
	c := Config{Token: strings.Repeat("x", 32), Interval: time.Minute, HistoryFile: filepath.Join(t.TempDir(), "data/history.json"), Routes: []Route{{ID: "eu/vless", Server: "eu", Protocol: "vless", Outbound: map[string]any{"password": "never-expose-secret"}}}}
	s, e := NewService(c)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func request(s *Service, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestAPIAuthenticationQueueAndRedaction(t *testing.T) {
	s := testService(t)
	for _, path := range []string{"/api/routes", "/api/history", "/metrics"} {
		if w := request(s, "GET", path, "", ""); w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	w := request(s, "GET", "/api/routes", s.cfg.Token, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "never-expose-secret") || strings.Contains(w.Body.String(), "outbound") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		body string
		want int
	}{{`{"route":"eu/vless","extra":true}`, 400}, {`{"route":"eu/vless"}{}`, 400}, {`{"route":"unknown"}`, 404}, {`{"route":"eu/vless"}`, 202}, {`{"route":"eu/vless"}`, 409}} {
		w = request(s, "POST", "/api/test", s.cfg.Token, tc.body)
		if w.Code != tc.want {
			t.Fatal(w.Code, tc)
		}
	}
	if !s.States()[0].Busy {
		t.Fatal("queued state missing")
	}
}
func TestPersistenceHistoryAndFreshness(t *testing.T) {
	s := testService(t)
	now := time.Now().UTC()
	s.save(Result{RouteID: "eu/vless", State: "healthy", Started: now, Finished: now, UDP: UDPResult{State: "unsupported"}})
	restored, e := NewService(s.cfg)
	if e != nil {
		t.Fatal(e)
	}
	if len(restored.History("eu/vless")) != 1 || restored.States()[0].Stale {
		t.Fatal("history not restored")
	}
	info, e := os.Stat(s.cfg.HistoryFile)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("history permissions", info, e)
	}
	restored.save(Result{RouteID: "eu/vless", Finished: now.Add(-4 * time.Minute), State: "healthy"})
	if !restored.States()[0].Stale {
		t.Fatal("stale result appears fresh")
	}
}
func TestPersistenceFailureVisibleWithoutDroppingResult(t *testing.T) {
	s := testService(t)
	file := filepath.Join(t.TempDir(), "file")
	if e := os.WriteFile(file, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	s.cfg.HistoryFile = filepath.Join(file, "history.json")
	s.save(Result{RouteID: "eu/vless", Finished: time.Now(), State: "down"})
	if len(s.History("eu/vless")) != 1 {
		t.Fatal("result lost")
	}
	w := request(s, "GET", "/metrics", s.cfg.Token, "")
	if !strings.Contains(w.Body.String(), "vpn_monitor_persistence_error 1") {
		t.Fatal(w.Body.String())
	}
}
func TestMetricsDoNotInventUnsupportedStatistics(t *testing.T) {
	s := testService(t)
	s.save(Result{RouteID: "eu/vless", Finished: time.Now(), State: "healthy", UDP: UDPResult{State: "unsupported"}, Speed: SpeedResult{State: "disabled"}})
	body := request(s, "GET", "/metrics", s.cfg.Token, "").Body.String()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "vpn_monitor_udp_loss_ratio{") || strings.HasPrefix(line, "vpn_monitor_speed_bits_per_second{") {
			t.Fatal("invented measurement", line)
		}
	}
	if !strings.Contains(body, `vpn_monitor_healthy{server="eu",protocol="vless"} 1`) {
		t.Fatal(body)
	}
	s.save(Result{RouteID: "eu/vless", Finished: time.Now().Add(-time.Hour), State: "healthy"})
	body = request(s, "GET", "/metrics", s.cfg.Token, "").Body.String()
	if !strings.Contains(body, `vpn_monitor_healthy{server="eu",protocol="vless"} 0`) {
		t.Fatal("stale marked healthy")
	}
}
func TestConcurrentQueueDeduplicates(t *testing.T) {
	s := testService(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Enqueue("eu/vless") == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
			_ = s.States()
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatal(accepted)
	}
}
func TestHistoryEndpointJSON(t *testing.T) {
	s := testService(t)
	s.save(Result{RouteID: "eu/vless", Finished: time.Now(), State: "down"})
	w := request(s, "GET", "/api/history?route=eu%2Fvless", s.cfg.Token, "")
	var results []Result
	if json.Unmarshal(w.Body.Bytes(), &results) != nil || len(results) != 1 {
		t.Fatal(w.Body.String())
	}
}
func TestCorruptHistoryFailsClosed(t *testing.T) {
	s := testService(t)
	if e := os.MkdirAll(filepath.Dir(s.cfg.HistoryFile), 0700); e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(s.cfg.HistoryFile, []byte("not-json"), 0600)
	if _, e := NewService(s.cfg); e == nil {
		t.Fatal("corrupt history accepted")
	}
}
func TestWorkerCancelsAndFinishes(t *testing.T) {
	s := testService(t)
	s.cfg.SingBox = "/missing"
	s.cfg.Timeout = 10 * time.Millisecond
	s.cfg.Samples = 1
	s.cfg.Controls = []Target{{Name: "control", URL: "https://127.0.0.1:1/", Status: 200}}
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not finish")
	}
}
func TestMetricsAndHealthMethod(t *testing.T) {
	s := testService(t)
	if w := request(s, "GET", "/healthz", "", ""); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	if w := request(s, "POST", "/api/routes", s.cfg.Token, ""); w.Code != 405 {
		t.Fatal(fmt.Sprint(w.Code))
	}
}
