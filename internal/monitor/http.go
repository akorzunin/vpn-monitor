package monitor

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/routes", func(w http.ResponseWriter, r *http.Request) { respond(w, s.States()) })
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) { respond(w, s.History(r.URL.Query().Get("route"))) })
	mux.HandleFunc("POST /api/test", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Route string `json:"route"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		d.DisallowUnknownFields()
		var extra any
		if d.Decode(&input) != nil || d.Decode(&extra) != io.EOF {
			http.Error(w, "invalid request", 400)
			return
		}
		if err := s.Enqueue(input.Route); err != nil {
			code := 404
			if err == ErrBusy {
				code = 409
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /metrics", s.metrics)
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.cfg.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})
	root := http.NewServeMux()
	root.Handle("/api/", protected)
	root.Handle("/metrics", protected)
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok\n")) })
	root.Handle("/", http.FileServer(http.Dir(s.cfg.WebDir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		// No CORS: browser control requests must be same-origin and use a bearer header.
		root.ServeHTTP(w, r)
	})
}
func respond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Service) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	gauge := func(name, help string) { fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	gauge("vpn_monitor_last_run_timestamp_seconds", "Last completed test; inspect freshness before interpreting health.")
	gauge("vpn_monitor_healthy", "1 only for a healthy completed run; stale runs are not healthy.")
	gauge("vpn_monitor_http_failure_ratio", "Failed HTTP requests / total HTTP requests; not packet loss.")
	gauge("vpn_monitor_http_latency_seconds", "Mean HTTP time to response headers of successful samples.")
	gauge("vpn_monitor_http_p95_seconds", "P95 HTTP time to response headers of successful samples.")
	gauge("vpn_monitor_udp_loss_ratio", "Authenticated UDP echo packet loss; emitted only when packets were sent.")
	gauge("vpn_monitor_udp_rtt_seconds", "Mean successful UDP echo round-trip time.")
	gauge("vpn_monitor_speed_bits_per_second", "Sampled download rate, emitted only for a successful speed test.")
	gauge("vpn_monitor_busy", "Route queued or running.")
	gauge("vpn_monitor_state", "One-hot latest result state; freshness is independent.")
	for _, state := range s.States() {
		labels := fmt.Sprintf("server=%q,protocol=%q", state.Server, state.Protocol)
		busy := 0
		if state.Busy {
			busy = 1
		}
		fmt.Fprintf(w, "vpn_monitor_busy{%s} %d\n", labels, busy)
		if state.Latest == nil {
			fmt.Fprintf(w, "vpn_monitor_last_run_timestamp_seconds{%s} 0\nvpn_monitor_healthy{%s} 0\nvpn_monitor_state{%s,state=\"unknown\"} 1\n", labels, labels, labels)
			continue
		}
		v := state.Latest
		healthy := 0
		if v.State == "healthy" && !state.Stale {
			healthy = 1
		}
		fmt.Fprintf(w, "vpn_monitor_last_run_timestamp_seconds{%s} %d\nvpn_monitor_healthy{%s} %d\nvpn_monitor_state{%s,state=%q} 1\n", labels, v.Finished.Unix(), labels, healthy, labels, v.State)
		if len(v.Samples) > 0 {
			fmt.Fprintf(w, "vpn_monitor_http_failure_ratio{%s} %g\n", labels, v.FailurePercent/100)
		}
		if v.FailurePercent < 100 && len(v.Samples) > 0 {
			fmt.Fprintf(w, "vpn_monitor_http_latency_seconds{%s} %g\nvpn_monitor_http_p95_seconds{%s} %g\n", labels, v.LatencyMS/1000, labels, v.P95MS/1000)
		}
		if v.UDP.Sent > 0 {
			fmt.Fprintf(w, "vpn_monitor_udp_loss_ratio{%s} %g\n", labels, v.UDP.LossPercent/100)
		}
		if v.UDP.Received > 0 {
			fmt.Fprintf(w, "vpn_monitor_udp_rtt_seconds{%s} %g\n", labels, v.UDP.RTTMS/1000)
		}
		if v.Speed.State == "ok" {
			fmt.Fprintf(w, "vpn_monitor_speed_bits_per_second{%s} %g\n", labels, v.Speed.Mbps*1e6)
		}
	}
	s.mu.RLock()
	failed := s.persistenceError
	s.mu.RUnlock()
	value := 0
	if failed {
		value = 1
	}
	fmt.Fprintf(w, "# HELP vpn_monitor_persistence_error Last history write failed.\n# TYPE vpn_monitor_persistence_error gauge\nvpn_monitor_persistence_error %d\n", value)
}

// VerifyWeb prevents accidentally serving a project root (and config files) as static assets.
func VerifyWeb(dir string) error { _, err := os.Stat(filepath.Join(dir, "index.html")); return err }
