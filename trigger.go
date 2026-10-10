package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// TriggerConfig turns incoming messages into an outgoing webhook, so an
// agent can be woken when someone writes in a chat. The webhook fires only
// if no other device on this account has replied within Delay, which lets a
// primary responder (e.g. a desktop agent on another linked device) answer
// first and keeps this server as the fallback.
type TriggerConfig struct {
	URL     string        // https endpoint to POST to; empty = triggers off
	Token   string        // bearer token for URL
	Format  string        // "json" (default) or "claude-routine"
	Chats   AllowList     // chats that can trigger
	From    AllowList     // senders that can trigger
	Delay   time.Duration // wait this long for a reply from another device
	PerHour int           // max webhook calls per hour
}

const maxTriggerAge = 10 * time.Minute // ignore backlog delivered on reconnect

func loadTriggerConfig(c *Config) error {
	t := &c.Trigger
	t.URL = env("TRIGGER_URL", "")
	if t.URL == "" {
		return nil
	}
	u, err := url.Parse(t.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !isLoopbackHost(u.Hostname())) {
		return fmt.Errorf("TRIGGER_URL must be an https URL")
	}
	t.Token = os.Getenv("TRIGGER_TOKEN")
	if t.Token == "" {
		return fmt.Errorf("TRIGGER_TOKEN is required when TRIGGER_URL is set")
	}
	t.Format = env("TRIGGER_FORMAT", "json")
	if t.Format != "json" && t.Format != "claude-routine" {
		return fmt.Errorf("TRIGGER_FORMAT must be json or claude-routine")
	}
	if t.Chats, err = ParseAllowList(os.Getenv("TRIGGER_CHATS")); err != nil {
		return fmt.Errorf("TRIGGER_CHATS: %w", err)
	}
	if t.From, err = ParseAllowList(os.Getenv("TRIGGER_FROM")); err != nil {
		return fmt.Errorf("TRIGGER_FROM: %w", err)
	}
	if t.Chats.Empty() || t.Chats.All || t.From.Empty() || t.From.All {
		return fmt.Errorf("TRIGGER_CHATS and TRIGGER_FROM must list specific chats and senders (no *)")
	}
	for j := range t.Chats.JIDs {
		jid, _ := types.ParseJID(j)
		if !c.ReadAllow.Allows(jid) {
			return fmt.Errorf("TRIGGER_CHATS entry %s must also be in WA_READ_ALLOW", j)
		}
	}
	secs, err := strconv.Atoi(env("TRIGGER_DELAY_SECONDS", "90"))
	if err != nil || secs < 0 || secs > 3600 {
		return fmt.Errorf("TRIGGER_DELAY_SECONDS must be 0-3600")
	}
	t.Delay = time.Duration(secs) * time.Second
	if t.PerHour, err = strconv.Atoi(env("TRIGGER_PER_HOUR", "20")); err != nil || t.PerHour < 1 {
		return fmt.Errorf("TRIGGER_PER_HOUR must be a positive number")
	}
	return nil
}

type Trigger struct {
	cfg  TriggerConfig
	st   *Store
	log  *slog.Logger
	rate *RateLimiter
	post func(ctx context.Context, body []byte) error
	now  func() time.Time

	mu      sync.Mutex
	pending map[string]*time.Timer // chat -> timer waiting for a reply
	first   map[string]int64       // chat -> unix time of the first unanswered message
}

func NewTrigger(cfg TriggerConfig, st *Store, log *slog.Logger) *Trigger {
	t := &Trigger{cfg: cfg, st: st, log: log, rate: &RateLimiter{limit: cfg.PerHour}, now: time.Now,
		pending: map[string]*time.Timer{}, first: map[string]int64{}}
	t.post = t.httpPost
	return t
}

// Observe is called for every stored live message.
func (t *Trigger) Observe(chat, sender types.JID, fromMe bool, ts time.Time) {
	if t == nil || !t.cfg.Chats.Allows(chat) || t.now().Sub(ts) > maxTriggerAge {
		return
	}
	key := chat.ToNonAD().String()
	t.mu.Lock()
	defer t.mu.Unlock()
	if fromMe {
		// Another device (or this server) replied: nothing to do.
		if tm, ok := t.pending[key]; ok {
			tm.Stop()
			delete(t.pending, key)
			delete(t.first, key)
			t.log.Info("trigger skipped: answered by another device", "chat", key)
		}
		return
	}
	if !t.cfg.From.Allows(sender) {
		return
	}
	if _, ok := t.pending[key]; ok {
		return // already waiting; later messages ride along in the same call
	}
	t.first[key] = ts.Unix()
	t.pending[key] = time.AfterFunc(t.cfg.Delay, func() { t.fire(key) })
}

func (t *Trigger) fire(chat string) {
	t.mu.Lock()
	first, ok := t.first[chat]
	delete(t.pending, chat)
	delete(t.first, chat)
	t.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msgs, err := t.st.Messages(ctx, chat, 20, 0, 0)
	if err != nil {
		t.log.Error("trigger: read history", "err", err)
		return
	}
	for _, m := range msgs { // re-check in case a reply arrived before the event did
		if m.FromMe && m.ts >= first {
			t.log.Info("trigger skipped: answered by another device", "chat", chat)
			return
		}
	}
	if err := t.rate.Take(); err != nil {
		t.log.Warn("trigger skipped: hourly limit reached", "chat", chat, "limit", t.cfg.PerHour)
		return
	}
	body, err := t.payload(ctx, chat, first, msgs)
	if err == nil {
		err = t.post(ctx, body)
	}
	if err != nil {
		t.log.Error("trigger failed", "chat", chat, "err", err)
		return
	}
	t.log.Info("trigger fired", "chat", chat)
}

func (t *Trigger) payload(ctx context.Context, chat string, first int64, newestFirst []StoredMessage) ([]byte, error) {
	name := t.st.ChatName(ctx, chat)
	if t.cfg.Format == "json" {
		return json.Marshal(map[string]any{"chat": chat, "chat_name": name, "since": first, "messages": newestFirst})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "WhatsApp message(s) in %q (chat_id %s) got no reply within %s.\n", name, chat, t.cfg.Delay)
	b.WriteString("Recent messages, oldest first. NEW = not answered yet; me = this WhatsApp account.\n\n")
	for i := len(newestFirst) - 1; i >= 0; i-- {
		m := newestFirst[i]
		who := m.SenderName
		if m.FromMe {
			who = "me"
		} else if who == "" {
			who = m.SenderJID
		}
		tag := ""
		if !m.FromMe && m.ts >= first {
			tag = "NEW "
		}
		text := m.Text
		if m.MediaType != "" {
			text = strings.TrimSpace("[" + m.MediaType + "] " + text)
		}
		if r := []rune(text); len(r) > 1500 {
			text = string(r[:1500]) + "…"
		}
		fmt.Fprintf(&b, "%s[%s] %s (msg %s): %s\n", tag, m.Time, who, m.ID, text)
	}
	return json.Marshal(map[string]string{"text": b.String()})
}

func (t *Trigger) httpPost(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.cfg.Token)
	if t.cfg.Format == "claude-routine" {
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "experimental-cc-routine-2026-04-01")
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
