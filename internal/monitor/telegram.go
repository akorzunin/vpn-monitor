package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func (s *Service) telegram(ctx context.Context) {
	client := httpClient("", 35*time.Second)
	defer client.CloseIdleConnections()
	allowed := map[int64]bool{}
	for _, id := range s.cfg.Telegram.ChatIDs {
		allowed[id] = true
	}
	call := func(method string, input any, output any) error {
		b, _ := json.Marshal(input)
		req, e := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+s.cfg.Telegram.Token+"/"+method, bytes.NewReader(b))
		if e != nil {
			return fmt.Errorf("request setup failed")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, e := client.Do(req)
		if e != nil {
			return fmt.Errorf("Telegram unavailable")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("Telegram response unsuccessful")
		}
		var envelope struct {
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope) != nil || !envelope.OK {
			return fmt.Errorf("Telegram API rejected request")
		}
		if output != nil {
			return json.Unmarshal(envelope.Result, output)
		}
		return nil
	}
	send := func(chat int64, text string) {
		// Telegram limits messages to 4096 characters. Split at line boundaries using runes.
		for len(text) > 0 {
			runes := []rune(text)
			n := min(len(runes), 3500)
			if e := call("sendMessage", map[string]any{"chat_id": chat, "text": string(runes[:n]), "link_preview_options": map[string]bool{"is_disabled": true}}, nil); e != nil {
				log.Print("Telegram delivery failed; results are retained locally")
				return
			}
			text = string(runes[n:])
		}
	}
	// Polling/delivery run separately from checks; a blocked Telegram connection cannot block the worker or web UI.
	go func() {
		last := map[string]string{}
		for {
			select {
			case <-ctx.Done():
				return
			case result := <-s.notify:
				previous, known := last[result.RouteID]
				last[result.RouteID] = result.State
				if (!known && result.State == "healthy") || (known && previous == result.State) {
					continue
				}
				text := fmt.Sprintf("%s: %s\nHTTP %.1fms, %.1f%% failed\nUDP %s (%.1f%% loss, %.1fms RTT)\nSpeed %s: %.2f Mbps", result.RouteID, result.State, result.LatencyMS, result.FailurePercent, result.UDP.State, result.UDP.LossPercent, result.UDP.RTTMS, result.Speed.State, result.Speed.Mbps)
				for chat := range allowed {
					send(chat, text)
				}
			}
		}
	}()
	offset := int64(0)
	started := time.Now().Unix()
	for ctx.Err() == nil {
		var updates []struct {
			ID      int64 `json:"update_id"`
			Message *struct {
				Text string `json:"text"`
				Date int64  `json:"date"`
				Chat struct {
					ID   int64  `json:"id"`
					Type string `json:"type"`
				} `json:"chat"`
			} `json:"message"`
		}
		err := call("getUpdates", map[string]any{"offset": offset, "timeout": 25, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			log.Print("Telegram polling unavailable; web UI and checks continue")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			offset = u.ID + 1
			if u.Message == nil || u.Message.Date < started {
				continue
			}
			if reply := s.telegramReply(u.Message.Chat.ID, u.Message.Chat.Type, u.Message.Text); reply != "" {
				send(u.Message.Chat.ID, reply)
			}
		}
	}
}

func (s *Service) telegramReply(chat int64, chatType, text string) string {
	allowed := false
	for _, id := range s.cfg.Telegram.ChatIDs {
		if id == chat {
			allowed = true
		}
	}
	if !allowed || chatType != "private" {
		return ""
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	switch strings.Split(fields[0], "@")[0] {
	case "/start", "/help":
		return "/status — current states\n/test server/protocol — queue a full test\n/stats server/protocol — latest detailed result\nOnly explicitly allowed private chats can control this bot."
	case "/status":
		return s.Summary()
	case "/test":
		if len(fields) != 2 {
			return "Usage: /test server/protocol"
		}
		if err := s.Enqueue(fields[1]); err != nil {
			return err.Error()
		}
		return "Queued " + fields[1] + ". Use /stats " + fields[1] + " for results."
	case "/stats":
		if len(fields) != 2 {
			return "Usage: /stats server/protocol"
		}
		history := s.History(fields[1])
		if len(history) == 0 {
			return "No result"
		}
		b, _ := json.MarshalIndent(history[0], "", "  ")
		return string(b)
	}
	return ""
}
