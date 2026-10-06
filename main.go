package main

import (
	"context"
	"encoding/base64"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	qrcode "github.com/skip2/go-qrcode"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}
	level := slog.LevelInfo
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("config error", "err", err)
		os.Exit(1)
	}
	if strings.EqualFold(cfg.LogLevel, "DEBUG") {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, dialect, err := OpenDB(cfg.DatabaseURL)
	if err != nil {
		log.Error("database error", "err", err)
		os.Exit(1)
	}
	st, err := NewStore(ctx, db, dialect)
	if err != nil {
		log.Error("database setup failed", "err", err)
		os.Exit(1)
	}
	wa, err := NewWA(ctx, cfg, db, dialect, st, log)
	if err != nil {
		log.Error("whatsapp setup failed", "err", err)
		os.Exit(1)
	}
	if err := wa.Start(ctx); err != nil {
		log.Error("whatsapp connect failed", "err", err)
		os.Exit(1)
	}

	guard := &LoginGuard{}
	oauth := &OAuth{cfg: cfg, st: st, log: log, guard: guard}
	server := NewMCPServer(&tools{wa: wa, st: st, cfg: cfg, rate: &RateLimiter{limit: cfg.SendPerHour}})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	mux := http.NewServeMux()
	oauth.Routes(mux)
	mux.Handle("/mcp", oauth.Protect(mcpHandler))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		state, _, _ := wa.LinkState()
		writeJSON(w, 200, map[string]string{"status": "ok", "whatsapp": state})
	})
	mux.HandleFunc("GET /admin", adminPage(cfg, wa, guard))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("whatsapp-mcp-server " + version + "\nMCP endpoint: " + cfg.PublicURL + "/mcp\nAdmin: " + cfg.PublicURL + "/admin\n"))
	})

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				st.PurgeExpired(ctx)
			}
		}
	}()

	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}
	go func() {
		log.Info("listening", "addr", cfg.Listen, "public_url", cfg.PublicURL,
			"send_allow", cfg.SendAllow.String(), "read_allow", cfg.ReadAllow.String())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	wa.cli.Disconnect()
}

var adminTmpl = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
{{if eq .State "unlinked"}}<meta http-equiv="refresh" content="5">{{end}}<title>WhatsApp MCP admin</title>
<style>body{font-family:system-ui,sans-serif;max-width:640px;margin:32px auto;padding:0 16px;color:#111}
code{background:#f3f3f3;padding:2px 5px;border-radius:4px;word-break:break-all}td{padding:4px 8px;border-bottom:1px solid #eee}
.code{font-size:32px;letter-spacing:4px;font-weight:700}</style></head><body>
<h2>WhatsApp MCP server</h2>
<p>Status: <b>{{.State}}</b>{{if .Account}} as <code>{{.Account}}</code>{{end}}</p>
{{if eq .State "unlinked"}}
  {{if .Pair}}<p>On the phone: WhatsApp → Linked devices → Link a device → <b>Link with phone number instead</b>, then enter:</p><p class="code">{{.Pair}}</p>
  {{else if .QR}}<p>On the phone: WhatsApp → Linked devices → Link a device, then scan:</p><img alt="QR code" src="data:image/png;base64,{{.QR}}" width="280" height="280">
  {{else}}<p>Waiting for a code from WhatsApp…</p>{{end}}
  <p>This page refreshes every 5 seconds.</p>
{{end}}
<p>Send allow-list: <code>{{.Send}}</code><br>Read allow-list: <code>{{.Read}}</code></p>
<p>MCP URL for Claude: <code>{{.URL}}/mcp</code></p>
{{if .Groups}}<h3>Your groups</h3><table>{{range .Groups}}<tr><td>{{.Name}}</td><td><code>{{.JID}}</code></td><td>{{if .CanSend}}send {{end}}{{if .CanRead}}read{{end}}</td></tr>{{end}}</table>{{end}}
</body></html>`))

// adminPage shows link status, the QR/pairing code, and group JIDs (to fill
// in the allow-lists). HTTP Basic auth with ADMIN_PASSWORD, any username.
func adminPage(cfg *Config, wa *WA, guard *LoginGuard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		_, pass, ok := r.BasicAuth()
		if ok {
			ok, _ = guard.Check(pass, cfg.AdminPassword)
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="whatsapp-mcp admin", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		state, qr, pair := wa.LinkState()
		view := map[string]any{"State": state, "Account": wa.Account(), "Pair": pair, "URL": cfg.PublicURL,
			"Send": cfg.SendAllow.String(), "Read": cfg.ReadAllow.String()}
		if qr != "" {
			if png, err := qrcode.Encode(qr, qrcode.Medium, 280); err == nil {
				view["QR"] = base64.StdEncoding.EncodeToString(png)
			}
		}
		if state == "connected" {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			if groups, err := wa.Groups(ctx); err == nil {
				view["Groups"] = groups
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = adminTmpl.Execute(w, view)
	}
}

// healthcheck is for Docker HEALTHCHECK (the image has no shell or curl).
func healthcheck() int {
	addr := env("LISTEN_ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/health")
	if err != nil || resp.StatusCode != 200 {
		return 1
	}
	return 0
}
