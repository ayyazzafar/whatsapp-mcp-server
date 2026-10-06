package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// WA wraps the whatsmeow client: linking, receiving, sending.
type WA struct {
	cli   *whatsmeow.Client
	cfg   *Config
	st    *Store
	log   *slog.Logger
	names sync.Map // group JIDs whose names were already fetched

	mu       sync.Mutex
	qrCode   string // current QR payload while unlinked
	pairCode string // pairing code when WA_PAIR_PHONE is set
}

func NewWA(ctx context.Context, cfg *Config, db *sql.DB, dialect string, st *Store, log *slog.Logger) (*WA, error) {
	store.SetOSInfo(cfg.DeviceName, [3]uint32{1, 0, 0})
	container := sqlstore.NewWithDB(db, dialect, waLog.Stdout("store", "WARN", false))
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("whatsmeow store upgrade: %w", err)
	}
	dev, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}
	w := &WA{cfg: cfg, st: st, log: log}
	w.cli = whatsmeow.NewClient(dev, waLog.Stdout("whatsapp", cfg.LogLevel, false))
	w.cli.AddEventHandler(w.handle)
	return w, nil
}

// Start connects, or begins linking if no session is stored yet.
func (w *WA) Start(ctx context.Context) error {
	if w.cli.Store.ID != nil {
		return w.cli.Connect()
	}
	go w.linkLoop(ctx)
	return nil
}

func (w *WA) linkLoop(ctx context.Context) {
	for ctx.Err() == nil && w.cli.Store.ID == nil {
		qrChan, err := w.cli.GetQRChannel(ctx)
		if err != nil {
			w.log.Error("cannot get QR channel", "err", err)
			return
		}
		if err := w.cli.Connect(); err != nil {
			w.log.Error("connect failed", "err", err)
			time.Sleep(10 * time.Second)
			continue
		}
		asked := false
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				w.mu.Lock()
				w.qrCode = evt.Code
				w.mu.Unlock()
				if w.cfg.PairPhone != "" && !asked {
					asked = true
					code, err := w.cli.PairPhone(ctx, w.cfg.PairPhone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
					if err != nil {
						w.log.Error("pairing code request failed", "err", err)
					} else {
						w.mu.Lock()
						w.pairCode = code
						w.mu.Unlock()
						w.log.Info("PAIRING CODE: on the phone open WhatsApp > Linked devices > Link a device > Link with phone number instead", "code", code)
					}
				}
				if w.cfg.PairPhone == "" {
					w.log.Info("Scan this QR with WhatsApp > Linked devices > Link a device (or open /admin)")
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				}
			case "success":
				w.log.Info("linked to WhatsApp")
			default:
				w.log.Info("link status", "event", evt.Event)
			}
		}
		w.mu.Lock()
		w.qrCode, w.pairCode = "", ""
		w.mu.Unlock()
		if w.cli.Store.ID == nil {
			w.cli.Disconnect()
			w.log.Info("link attempt expired, starting a new one")
			time.Sleep(3 * time.Second)
		}
	}
}

// LinkState is shown on /admin and /health.
func (w *WA) LinkState() (state, qr, pair string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.cli.Store.ID == nil:
		return "unlinked", w.qrCode, w.pairCode
	case w.cli.IsConnected() && w.cli.IsLoggedIn():
		return "connected", "", ""
	default:
		return "disconnected", "", ""
	}
}

func (w *WA) Account() string {
	if w.cli.Store.ID == nil {
		return ""
	}
	return w.cli.Store.ID.ToNonAD().String()
}

// normalize maps hidden-user (LID) chat JIDs to phone-number JIDs where the
// mapping is known, so allow-lists written with phone numbers keep working.
func (w *WA) normalize(ctx context.Context, jid types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server == types.HiddenUserServer {
		if pn, err := w.cli.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return jid
}

func (w *WA) handle(raw any) {
	ctx := context.Background()
	switch evt := raw.(type) {
	case *events.Message:
		w.storeMessage(ctx, evt)
	case *events.HistorySync:
		for _, conv := range evt.Data.GetConversations() {
			chat, err := types.ParseJID(conv.GetID())
			if err != nil || !w.cfg.ReadAllow.Allows(w.normalize(ctx, chat)) {
				continue
			}
			for _, hm := range conv.GetMessages() {
				if m, err := w.cli.ParseWebMessage(chat, hm.GetMessage()); err == nil {
					w.storeMessage(ctx, m)
				}
			}
		}
	case *events.Connected:
		w.log.Info("connected to WhatsApp", "account", w.Account())
		go w.refreshAllowedNames(ctx)
	case *events.LoggedOut:
		w.log.Error("LOGGED OUT by WhatsApp: the device was unlinked or the session expired. Delete the session (or the database) and link again.", "reason", evt.Reason.String())
	case *events.StreamReplaced:
		w.log.Error("another client took over this session")
	case *events.ClientOutdated:
		w.log.Error("WhatsApp says this client is outdated: update the image (whatsmeow bump)")
	}
}

func (w *WA) storeMessage(ctx context.Context, evt *events.Message) {
	chat := w.normalize(ctx, evt.Info.Chat)
	if !w.cfg.ReadAllow.Allows(chat) || evt.Message == nil {
		return
	}
	text, media, filename := extractContent(evt.Message)
	if text == "" && media == "" {
		return // receipts, protocol messages, key updates, ...
	}
	raw, _ := proto.Marshal(evt.Message)
	sender := w.normalize(ctx, evt.Info.Sender)
	m := &StoredMessage{
		ChatJID: chat.String(), ID: evt.Info.ID, SenderJID: sender.String(), SenderName: evt.Info.PushName,
		FromMe: evt.Info.IsFromMe, Text: text, MediaType: media, Filename: filename,
		ts: evt.Info.Timestamp.Unix(), raw: raw,
	}
	if err := w.st.SaveMessage(ctx, m); err != nil {
		w.log.Error("store message", "err", err)
		return
	}
	if chat.Server == types.GroupServer {
		w.ensureGroupName(ctx, chat)
	} else if !evt.Info.IsFromMe && evt.Info.PushName != "" && w.st.ChatName(ctx, chat.String()) == "" {
		_ = w.st.SetChatName(ctx, chat.String(), evt.Info.PushName)
	}
}

func (w *WA) ensureGroupName(ctx context.Context, jid types.JID) {
	if _, done := w.names.LoadOrStore(jid.String(), true); done {
		return
	}
	go func() {
		info, err := w.cli.GetGroupInfo(ctx, jid)
		if err != nil {
			w.names.Delete(jid.String())
			return
		}
		_ = w.st.SetChatName(ctx, jid.String(), info.Name)
	}()
}

func (w *WA) refreshAllowedNames(ctx context.Context) {
	for _, list := range []AllowList{w.cfg.SendAllow, w.cfg.ReadAllow} {
		for j := range list.JIDs {
			if jid, err := types.ParseJID(j); err == nil && jid.Server == types.GroupServer {
				w.ensureGroupName(ctx, jid)
			}
		}
	}
}

func extractContent(m *waE2E.Message) (text, media, filename string) {
	switch {
	case m.GetConversation() != "":
		return m.GetConversation(), "", ""
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText(), "", ""
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption(), "image", ""
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetCaption(), "video", ""
	case m.GetAudioMessage() != nil:
		if m.GetAudioMessage().GetPTT() {
			return "", "voice", ""
		}
		return "", "audio", ""
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		return d.GetCaption(), "document", d.GetFileName()
	case m.GetStickerMessage() != nil:
		return "", "sticker", ""
	case m.GetReactionMessage() != nil:
		r := m.GetReactionMessage()
		if r.GetText() == "" {
			return "", "", ""
		}
		return fmt.Sprintf("[reacted %s to message %s]", r.GetText(), r.GetKey().GetID()), "", ""
	case m.GetLocationMessage() != nil:
		l := m.GetLocationMessage()
		return fmt.Sprintf("[location %f,%f %s]", l.GetDegreesLatitude(), l.GetDegreesLongitude(), l.GetName()), "", ""
	case m.GetContactMessage() != nil:
		return "[contact card: " + m.GetContactMessage().GetDisplayName() + "]", "", ""
	case m.GetPollCreationMessage() != nil:
		return "[poll] " + m.GetPollCreationMessage().GetName(), "", ""
	}
	return "", "", ""
}

var errNotLinked = errors.New("WhatsApp is not linked yet: open /admin on the server to link a device")

func (w *WA) ready() error {
	if w.cli.Store.ID == nil {
		return errNotLinked
	}
	if !w.cli.IsConnected() {
		return errors.New("WhatsApp is disconnected right now; it reconnects automatically, try again shortly")
	}
	return nil
}

// recordSent stores our own outgoing message so read_messages shows both sides.
func (w *WA) recordSent(ctx context.Context, chat types.JID, id string, ts time.Time, msg *waE2E.Message) {
	if !w.cfg.ReadAllow.Allows(chat) {
		return
	}
	text, media, filename := extractContent(msg)
	raw, _ := proto.Marshal(msg)
	_ = w.st.SaveMessage(ctx, &StoredMessage{
		ChatJID: chat.String(), ID: id, SenderJID: w.Account(), SenderName: "me", FromMe: true,
		Text: text, MediaType: media, Filename: filename, ts: ts.Unix(), raw: raw,
	})
}

func (w *WA) SendText(ctx context.Context, chat types.JID, text, replyTo string) (string, error) {
	if err := w.ready(); err != nil {
		return "", err
	}
	msg := &waE2E.Message{Conversation: proto.String(text)}
	if replyTo != "" {
		q, err := w.st.GetMessage(ctx, chat.String(), replyTo)
		if err != nil {
			return "", err
		}
		quoted := &waE2E.Message{}
		if err := proto.Unmarshal(q.raw, quoted); err != nil {
			return "", err
		}
		msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID: proto.String(q.ID), Participant: proto.String(q.SenderJID), QuotedMessage: quoted,
			},
		}}
	}
	resp, err := w.cli.SendMessage(ctx, chat, msg)
	if err != nil {
		return "", err
	}
	w.recordSent(ctx, chat, resp.ID, resp.Timestamp, msg)
	return resp.ID, nil
}

func (w *WA) SendFile(ctx context.Context, chat types.JID, data []byte, filename, caption string) (string, error) {
	if err := w.ready(); err != nil {
		return "", err
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	kind := whatsmeow.MediaDocument
	switch {
	case strings.HasPrefix(mimeType, "image/") && mimeType != "image/svg+xml" && mimeType != "image/gif":
		kind = whatsmeow.MediaImage
	case strings.HasPrefix(mimeType, "video/"):
		kind = whatsmeow.MediaVideo
	case strings.HasPrefix(mimeType, "audio/"):
		kind = whatsmeow.MediaAudio
	}
	up, err := w.cli.Upload(ctx, data, kind)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	size := uint64(len(data))
	var msg *waE2E.Message
	switch kind {
	case whatsmeow.MediaImage:
		msg = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String(caption), Mimetype: proto.String(mimeType),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &size}}
	case whatsmeow.MediaVideo:
		msg = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String(caption), Mimetype: proto.String(mimeType),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &size}}
	case whatsmeow.MediaAudio:
		msg = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String(mimeType),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &size}}
	default:
		msg = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String(caption), Mimetype: proto.String(mimeType), FileName: proto.String(filename),
			Title: proto.String(filename), URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &size}}
	}
	resp, err := w.cli.SendMessage(ctx, chat, msg)
	if err != nil {
		return "", err
	}
	w.recordSent(ctx, chat, resp.ID, resp.Timestamp, msg)
	return resp.ID, nil
}

func (w *WA) React(ctx context.Context, chat types.JID, msgID, emoji string) error {
	if err := w.ready(); err != nil {
		return err
	}
	m, err := w.st.GetMessage(ctx, chat.String(), msgID)
	if err != nil {
		return err
	}
	sender, err := types.ParseJID(m.SenderJID)
	if err != nil {
		return err
	}
	if m.FromMe {
		sender = *w.cli.Store.ID
	}
	_, err = w.cli.SendMessage(ctx, chat, w.cli.BuildReaction(chat, sender, msgID, emoji))
	return err
}

func (w *WA) Download(ctx context.Context, chat types.JID, msgID string) ([]byte, *StoredMessage, string, error) {
	if err := w.ready(); err != nil {
		return nil, nil, "", err
	}
	m, err := w.st.GetMessage(ctx, chat.String(), msgID)
	if err != nil {
		return nil, nil, "", err
	}
	if m.MediaType == "" {
		return nil, nil, "", errors.New("that message has no media")
	}
	msg := &waE2E.Message{}
	if err := proto.Unmarshal(m.raw, msg); err != nil {
		return nil, nil, "", err
	}
	data, err := w.cli.DownloadAny(ctx, msg)
	if err != nil {
		return nil, nil, "", fmt.Errorf("download: %w", err)
	}
	mimeType := ""
	switch {
	case msg.GetImageMessage() != nil:
		mimeType = msg.GetImageMessage().GetMimetype()
	case msg.GetVideoMessage() != nil:
		mimeType = msg.GetVideoMessage().GetMimetype()
	case msg.GetAudioMessage() != nil:
		mimeType = msg.GetAudioMessage().GetMimetype()
	case msg.GetDocumentMessage() != nil:
		mimeType = msg.GetDocumentMessage().GetMimetype()
	case msg.GetStickerMessage() != nil:
		mimeType = msg.GetStickerMessage().GetMimetype()
	}
	return data, m, mimeType, nil
}

type GroupSummary struct {
	JID          string `json:"jid"`
	Name         string `json:"name"`
	Participants int    `json:"participants"`
	CanSend      bool   `json:"can_send"`
	CanRead      bool   `json:"can_read"`
}

func (w *WA) Groups(ctx context.Context) ([]GroupSummary, error) {
	if err := w.ready(); err != nil {
		return nil, err
	}
	groups, err := w.cli.GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GroupSummary, 0, len(groups))
	for _, g := range groups {
		out = append(out, GroupSummary{JID: g.JID.String(), Name: g.Name, Participants: len(g.Participants),
			CanSend: w.cfg.SendAllow.Allows(g.JID), CanRead: w.cfg.ReadAllow.Allows(g.JID)})
	}
	return out, nil
}
