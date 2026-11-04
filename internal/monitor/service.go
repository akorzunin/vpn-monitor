package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type RouteState struct {
	Route
	Busy   bool    `json:"busy"`
	Latest *Result `json:"latest"`
	Stale  bool    `json:"stale"`
}
type Service struct {
	cfg              Config
	mu               sync.RWMutex
	history          []Result
	busy             map[string]bool
	queue            chan Route
	notify           chan Result
	persistenceError bool
	workers          sync.WaitGroup
}

func NewService(cfg Config) (*Service, error) {
	s := &Service{cfg: cfg, busy: map[string]bool{}, queue: make(chan Route, len(cfg.Routes)), notify: make(chan Result, 64), history: []Result{}}
	b, e := os.ReadFile(cfg.HistoryFile)
	if e == nil {
		if len(b) > 64<<20 {
			return nil, errors.New("history exceeds 64MiB")
		}
		if json.Unmarshal(b, &s.history) != nil {
			return nil, errors.New("invalid history file; restore a backup or move it aside")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if len(s.history) > 1000 {
		s.history = s.history[len(s.history)-1000:]
	}
	return s, nil
}
func (s *Service) Enqueue(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.cfg.Routes {
		if r.ID == id {
			if s.busy[id] {
				return ErrBusy
			}
			s.busy[id] = true
			s.queue <- r
			return nil
		}
	}
	return errors.New("unknown route")
}
func (s *Service) States() []RouteState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	states := make([]RouteState, 0, len(s.cfg.Routes))
	for _, r := range s.cfg.Routes {
		state := RouteState{Route: r, Busy: s.busy[r.ID], Stale: true}
		for i := len(s.history) - 1; i >= 0; i-- {
			if s.history[i].RouteID == r.ID {
				v := s.history[i]
				state.Latest = &v
				state.Stale = time.Since(v.Finished) > 3*s.cfg.Interval
				break
			}
		}
		states = append(states, state)
	}
	return states
}
func (s *Service) History(id string) []Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := []Result{}
	for i := len(s.history) - 1; i >= 0 && len(result) < 100; i-- {
		if s.history[i].RouteID == id {
			result = append(result, s.history[i])
		}
	}
	return result
}
func (s *Service) save(r Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, r)
	if len(s.history) > 1000 {
		s.history = s.history[len(s.history)-1000:]
	}
	s.busy[r.RouteID] = false
	// Global history lock includes disk writes; move persistence outside the lock if API latency matters.
	// Small bounded JSON history; move to SQLite if larger retention or multiple writers are needed.
	b, _ := json.Marshal(s.history)
	dir := filepath.Dir(s.cfg.HistoryFile)
	err := os.MkdirAll(dir, 0700)
	if err == nil {
		err = os.WriteFile(s.cfg.HistoryFile+".tmp", b, 0600)
	}
	if err == nil {
		err = os.Rename(s.cfg.HistoryFile+".tmp", s.cfg.HistoryFile)
	}
	s.persistenceError = err != nil
	if err != nil {
		log.Print("history persistence failed; results remain in memory")
	}
	if s.cfg.Telegram.Token != "" {
		select {
		case s.notify <- r:
		default:
			log.Print("Telegram notification queue full; result remains in web UI and metrics")
		}
	}
}
func (s *Service) Start(ctx context.Context) {
	// Single worker avoids competing speed tests; use per-egress queues if throughput matters.
	s.workers.Add(2)
	go func() {
		defer s.workers.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case route := <-s.queue:
				if ctx.Err() != nil {
					return
				}
				s.save(Run(ctx, s.cfg, route))
			}
		}
	}()
	go func() {
		defer s.workers.Done()
		for _, r := range s.cfg.Routes {
			_ = s.Enqueue(r.ID)
		}
		ticker := time.NewTicker(s.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, r := range s.cfg.Routes {
					_ = s.Enqueue(r.ID)
				}
			}
		}
	}()
	if s.cfg.Telegram.Token != "" {
		go s.telegram(ctx)
	}
}

// Wait lets the active test cancel and its child process exit before shutdown.
func (s *Service) Wait() { s.workers.Wait() }

func (s *Service) Summary() string {
	text := "VPN monitor\n"
	for _, r := range s.States() {
		state := "unknown"
		detail := ""
		if r.Latest != nil {
			v := r.Latest
			state = v.State
			detail = fmt.Sprintf(" | HTTP %.0fms, %.0f%% failed | UDP %s | speed %s", v.LatencyMS, v.FailurePercent, v.UDP.State, v.Speed.State)
		}
		if r.Stale {
			state += " (stale)"
		}
		if r.Busy {
			state += " (queued/testing)"
		}
		text += r.ID + ": " + state + detail + "\n"
	}
	return text
}
