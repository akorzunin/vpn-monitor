package monitor

import (
	"strings"
	"testing"
	"time"
)

func TestTelegramAllowlistAndCommands(t *testing.T) {
	s := testService(t)
	s.cfg.Telegram.ChatIDs = []int64{123}
	for _, tc := range []struct {
		id   int64
		kind string
	}{{456, "private"}, {123, "group"}, {123, "supergroup"}} {
		if reply := s.telegramReply(tc.id, tc.kind, "/test eu/vless"); reply != "" {
			t.Fatal("unauthorized reply", reply)
		}
	}
	if s.States()[0].Busy {
		t.Fatal("unauthorized chat queued test")
	}
	if reply := s.telegramReply(123, "private", "/status@monitorbot"); !strings.Contains(reply, "eu/vless") {
		t.Fatal(reply)
	}
	if reply := s.telegramReply(123, "private", "/test eu/vless"); !strings.HasPrefix(reply, "Queued") {
		t.Fatal(reply)
	}
	if reply := s.telegramReply(123, "private", "/test eu/vless"); reply != ErrBusy.Error() {
		t.Fatal(reply)
	}
	s.save(Result{RouteID: "eu/vless", Finished: time.Now(), State: "down"})
	if reply := s.telegramReply(123, "private", "/stats eu/vless"); !strings.Contains(reply, `"state": "down"`) || strings.Contains(reply, "never-expose-secret") {
		t.Fatal(reply)
	}
	if reply := s.telegramReply(123, "private", "/test"); !strings.HasPrefix(reply, "Usage:") {
		t.Fatal(reply)
	}
}
