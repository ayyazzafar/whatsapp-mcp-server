package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Forward sends new messages from chosen groups to a webhook (e.g. the Claude bridge), so a reply
// on the phone can wake whoever owns that group. Off unless WA_FORWARD and FORWARD_URL are set.
type Forward struct {
	Targets map[string]string // group JID -> label passed on as "project"
	URL     string
	Token   string
}

// ParseForward reads WA_FORWARD: comma-separated "groupJID=label" pairs.
func ParseForward(s, url, token string) (Forward, error) {
	f := Forward{Targets: map[string]string{}, URL: url, Token: token}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		jidStr, label, ok := strings.Cut(item, "=")
		jid, err := types.ParseJID(strings.TrimSpace(jidStr))
		if !ok || err != nil || jid.Server != types.GroupServer || strings.TrimSpace(label) == "" {
			return f, fmt.Errorf("bad entry %q: use groupJID=label", item)
		}
		f.Targets[jid.ToNonAD().String()] = strings.TrimSpace(label)
	}
	if len(f.Targets) > 0 && !strings.HasPrefix(f.URL, "https://") {
		return f, fmt.Errorf("FORWARD_URL must be https when WA_FORWARD is set")
	}
	return f, nil
}

// forward posts one live message (never history, never our own) to the webhook in the background.
func (w *WA) forward(ctx context.Context, evt *events.Message) {
	f := w.cfg.Forward
	chat := w.normalize(ctx, evt.Info.Chat)
	label, ok := f.Targets[chat.String()]
	if !ok || evt.Info.IsFromMe || evt.Message == nil || time.Since(evt.Info.Timestamp) > 5*time.Minute {
		return
	}
	text, media, filename := extractContent(evt.Message)
	if text == "" && media != "" {
		text = "[" + media + " " + filename + "]"
	}
	if text == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"project": label, "chat": chat.String(), "chat_name": w.st.ChatName(ctx, chat.String()),
		"sender_name": evt.Info.PushName, "text": text, "id": evt.Info.ID,
		"time": evt.Info.Timestamp.UTC().Format(time.RFC3339),
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.URL, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if f.Token != "" {
			req.Header.Set("Authorization", "Bearer "+f.Token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.log.Error("forward failed", "chat", chat.String(), "err", err)
			return
		}
		resp.Body.Close()
		w.log.Info("forwarded", "chat", chat.String(), "to", label, "status", resp.StatusCode)
	}()
}
