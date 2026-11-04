package monitor

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Sample struct {
	Target     string  `json:"target"`
	OK         bool    `json:"ok"`
	HTTPStatus int     `json:"http_status"`
	LatencyMS  float64 `json:"latency_ms"`
	Error      string  `json:"error,omitempty"`
}
type UDPResult struct {
	State       string  `json:"state"`
	Sent        int     `json:"sent"`
	Received    int     `json:"received"`
	LossPercent float64 `json:"loss_percent"`
	RTTMS       float64 `json:"rtt_ms"`
	P95MS       float64 `json:"p95_ms"`
	Error       string  `json:"error,omitempty"`
}
type SpeedResult struct {
	State   string  `json:"state"`
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
	Mbps    float64 `json:"mbps"`
	Error   string  `json:"error,omitempty"`
}
type Result struct {
	RouteID        string      `json:"route_id"`
	Started        time.Time   `json:"started"`
	Finished       time.Time   `json:"finished"`
	State          string      `json:"state"`
	Error          string      `json:"error,omitempty"`
	Controls       []Sample    `json:"controls"`
	Samples        []Sample    `json:"samples"`
	FailurePercent float64     `json:"failure_percent"`
	LatencyMS      float64     `json:"latency_ms"`
	P95MS          float64     `json:"p95_ms"`
	UDP            UDPResult   `json:"udp"`
	Speed          SpeedResult `json:"speed"`
}

func latencies(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sort.Float64s(values)
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values)), values[int(math.Ceil(.95*float64(len(values))))-1]
}
func speedTest(ctx context.Context, client *http.Client, cfg SpeedConfig) SpeedResult {
	if cfg.URL == "" {
		return SpeedResult{State: "disabled"}
	}
	result := SpeedResult{State: "error"}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", cfg.URL, nil)
	if err != nil {
		result.Error = "invalid speed URL"
		return result
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-cache")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		result.Error = "speed request failed"
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		result.Error = "speed response must be uncompressed HTTP 200"
		return result
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "text/html") || strings.Contains(contentType, "json") || strings.Contains(contentType, "xml") {
		result.Error = "speed response is not a binary object"
		return result
	}
	// Bounded sampled download, not a sustained-capacity or media acceptance benchmark.
	result.Bytes, err = io.Copy(io.Discard, io.LimitReader(resp.Body, cfg.MaxBytes))
	result.Seconds = time.Since(start).Seconds()
	if result.Seconds > 0 {
		result.Mbps = float64(result.Bytes) * 8 / result.Seconds / 1e6
	}
	if err != nil {
		result.Error = "speed transfer interrupted"
		return result
	}
	if result.Bytes == 0 {
		result.Error = "empty speed response"
		return result
	}
	if resp.ContentLength >= 0 && result.Bytes < min(resp.ContentLength, cfg.MaxBytes) {
		result.Error = "truncated speed response"
		return result
	}
	result.State = "ok"
	return result
}
func Run(ctx context.Context, cfg Config, route Route) Result {
	result := Result{RouteID: route.ID, Started: time.Now().UTC(), State: "error", Samples: []Sample{}, Controls: []Sample{}, UDP: UDPResult{State: "unsupported"}, Speed: SpeedResult{State: "not_run"}}
	// A finite per-route ceiling includes startup, all HTTP requests, speed and UDP.
	budget := 6*time.Second + time.Duration(len(cfg.Controls)+len(cfg.Targets)*cfg.Samples)*cfg.Timeout + cfg.Speed.Timeout + time.Duration(cfg.UDP.Samples)*cfg.UDP.Timeout
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	direct := httpClient("", cfg.Timeout)
	defer direct.CloseIdleConnections()
	for _, target := range cfg.Controls {
		s, _ := getRequest(ctx, direct, target)
		result.Controls = append(result.Controls, s)
	}
	address, stop, err := startClient(ctx, cfg.SingBox, route)
	if err != nil {
		result.Error = err.Error()
		result.State = classify(result)
		result.Finished = time.Now().UTC()
		return result
	}
	defer stop()
	client := httpClient(address, cfg.Timeout)
	defer client.CloseIdleConnections()
	var latency []float64
	failures := 0
	for i := 0; i < cfg.Samples; i++ {
		for _, target := range cfg.Targets {
			sample, _ := getRequest(ctx, client, target)
			result.Samples = append(result.Samples, sample)
			if sample.OK {
				latency = append(latency, sample.LatencyMS)
			} else {
				failures++
			}
		}
	}
	result.FailurePercent = 100 * float64(failures) / float64(len(result.Samples))
	result.LatencyMS, result.P95MS = latencies(latency)
	speedClient := httpClient(address, cfg.Speed.Timeout)
	defer speedClient.CloseIdleConnections()
	result.Speed = speedTest(ctx, speedClient, cfg.Speed)
	if route.UDP {
		if cfg.UDP.Endpoint == "" {
			result.UDP.State = "disabled"
		} else {
			result.UDP = udpTest(ctx, address, cfg.UDP)
		}
	}
	result.State = classify(result)
	result.Finished = time.Now().UTC()
	return result
}
func classify(r Result) string {
	for _, c := range r.Controls {
		if !c.OK {
			return "inconclusive"
		}
	}
	if len(r.Samples) == 0 {
		return "error"
	}
	if r.FailurePercent == 100 {
		return "down"
	}
	if r.FailurePercent > 0 || r.UDP.State == "degraded" || r.UDP.State == "failed" || r.UDP.State == "error" || r.Speed.State == "error" {
		return "degraded"
	}
	return "healthy"
}

var ErrBusy = errors.New("route already queued or running")
