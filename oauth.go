package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// A minimal OAuth 2.1 authorization server for a single owner:
// dynamic client registration (RFC 7591), authorization code + PKCE (S256
// only), refresh-token rotation, and protected-resource metadata (RFC 9728),
// which is what claude.ai custom connectors expect. The consent page is
// unlocked with ADMIN_PASSWORD. Only token hashes are stored.

const (
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
	codeTTL    = 5 * time.Minute
	maxClients = 200
)

type OAuth struct {
	cfg   *Config
	st    *Store
	log   *slog.Logger
	guard *LoginGuard
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (o *OAuth) resource() string { return o.cfg.PublicURL + "/mcp" }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func (o *OAuth) Routes(mux *http.ServeMux) {
	prm := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		writeJSON(w, 200, map[string]any{
			"resource":                 o.resource(),
			"authorization_servers":    []string{o.cfg.PublicURL},
			"bearer_methods_supported": []string{"header"},
			"resource_name":            "WhatsApp MCP",
		})
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", prm)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", prm)
	asm := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		writeJSON(w, 200, map[string]any{
			"issuer":                                o.cfg.PublicURL,
			"authorization_endpoint":                o.cfg.PublicURL + "/authorize",
			"token_endpoint":                        o.cfg.PublicURL + "/token",
			"registration_endpoint":                 o.cfg.PublicURL + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
			"scopes_supported":                      []string{"whatsapp"},
		})
	}
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", asm)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/mcp", asm)
	mux.HandleFunc("POST /register", o.register)
	mux.HandleFunc("GET /authorize", o.authorize)
	mux.HandleFunc("POST /authorize", o.authorize)
	mux.HandleFunc("POST /token", o.token)
}

// redirectAllowed: https on an allow-listed host (or its subdomains), or
// http on loopback for local clients like Claude Code.
func (o *OAuth) redirectAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "http" && !isLoopbackHost(host) {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	for _, h := range o.cfg.RedirectHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func (o *OAuth) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		oauthError(w, 400, "invalid_client_metadata", "body must be JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, 400, "invalid_redirect_uri", "1 to 10 redirect_uris required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !o.redirectAllowed(u) {
			o.log.Warn("rejected client registration", "redirect_uri", u)
			oauthError(w, 400, "invalid_redirect_uri", "redirect host not allowed by OAUTH_REDIRECT_HOSTS: "+u)
			return
		}
	}
	if n, err := o.st.CountClients(r.Context()); err != nil || n >= maxClients {
		oauthError(w, 503, "temporarily_unavailable", "client registry is full")
		return
	}
	name := strings.TrimSpace(req.ClientName)
	if name == "" {
		name = "MCP client"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	id := "c_" + randomToken()
	uris, _ := json.Marshal(req.RedirectURIs)
	if err := o.st.AddClient(r.Context(), id, name, string(uris)); err != nil {
		oauthError(w, 500, "server_error", "could not save client")
		return
	}
	o.log.Info("registered OAuth client", "name", name, "redirects", req.RedirectURIs)
	writeJSON(w, 201, map[string]any{
		"client_id": id, "client_id_issued_at": time.Now().Unix(), "client_name": name,
		"redirect_uris": req.RedirectURIs, "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "token_endpoint_auth_method": "none",
	})
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Allow access to WhatsApp</title>
<style>body{font-family:system-ui,sans-serif;max-width:420px;margin:40px auto;padding:0 16px;color:#111}
.box{border:1px solid #ddd;border-radius:12px;padding:20px}input[type=password]{width:100%;padding:10px;font-size:16px;box-sizing:border-box;margin:8px 0 14px}
button{width:100%;padding:12px;font-size:16px;background:#128c4a;color:#fff;border:0;border-radius:8px}.err{color:#b00020}.muted{color:#666;font-size:14px}</style></head>
<body><div class="box"><h2>Allow access to WhatsApp?</h2>
<p><b>{{.Client}}</b> wants to use this WhatsApp MCP server{{if .Account}} (linked to {{.Account}}){{end}}.</p>
<p class="muted">It will return to <b>{{.Host}}</b>. Sending: {{.Send}}. Reading: {{.Read}}.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post">{{range $k, $v := .Params}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<label>Admin password<input type="password" name="password" autocomplete="current-password" autofocus required></label>
<button type="submit">Allow</button></form></div></body></html>`))

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self' https: http://localhost:* http://127.0.0.1:*; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

func (o *OAuth) authorize(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	q := r.Form
	clientID, redirect := q.Get("client_id"), q.Get("redirect_uri")
	name, uris, err := o.st.GetClient(r.Context(), clientID)
	var registered []string
	_ = json.Unmarshal([]byte(uris), &registered)
	// Never redirect to an unverified URI: show the error here instead.
	if err != nil || !slices.Contains(registered, redirect) {
		http.Error(w, "unknown client or unregistered redirect_uri", 400)
		return
	}
	back := func(params url.Values) {
		u, _ := url.Parse(redirect)
		v := u.Query()
		for k, vals := range params {
			v[k] = vals
		}
		if s := q.Get("state"); s != "" {
			v.Set("state", s)
		}
		v.Set("iss", o.cfg.PublicURL)
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if q.Get("response_type") != "code" {
		back(url.Values{"error": {"unsupported_response_type"}})
		return
	}
	challenge := q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" || len(challenge) < 43 || len(challenge) > 128 {
		back(url.Values{"error": {"invalid_request"}, "error_description": {"PKCE with S256 is required"}})
		return
	}
	if res := q.Get("resource"); res != "" && res != o.resource() && res != o.cfg.PublicURL {
		back(url.Values{"error": {"invalid_target"}})
		return
	}

	params := map[string]string{}
	for _, k := range []string{"client_id", "redirect_uri", "response_type", "code_challenge", "code_challenge_method", "state", "resource", "scope"} {
		if v := q.Get(k); v != "" {
			params[k] = v
		}
	}
	u, _ := url.Parse(redirect)
	view := map[string]any{"Client": name, "Host": u.Hostname(), "Params": params,
		"Send": o.cfg.SendAllow.String(), "Read": o.cfg.ReadAllow.String()}

	if r.Method == http.MethodGet {
		_ = consentPage.Execute(w, view)
		return
	}
	if ok, msg := o.guard.Check(r.FormValue("password"), o.cfg.AdminPassword); !ok {
		o.log.Warn("failed consent login", "client", name)
		view["Error"] = msg
		w.WriteHeader(http.StatusUnauthorized)
		_ = consentPage.Execute(w, view)
		return
	}
	code := randomToken()
	if err := o.st.SaveCode(r.Context(), hashToken(code), clientID, redirect, challenge, q.Get("resource"), time.Now().Add(codeTTL)); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	o.log.Info("authorized OAuth client", "client", name)
	back(url.Values{"code": {code}})
}

func (o *OAuth) issue(ctx context.Context, w http.ResponseWriter, client string) {
	access, refresh := randomToken(), randomToken()
	if o.st.SaveToken(ctx, hashToken(access), "access", client, time.Now().Add(accessTTL)) != nil ||
		o.st.SaveToken(ctx, hashToken(refresh), "refresh", client, time.Now().Add(refreshTTL)) != nil {
		oauthError(w, 500, "server_error", "could not issue token")
		return
	}
	writeJSON(w, 200, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(accessTTL.Seconds()),
		"refresh_token": refresh, "scope": "whatsapp",
	})
}

func (o *OAuth) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request", "form body required")
		return
	}
	ctx := r.Context()
	clientID := r.PostForm.Get("client_id")
	if clientID == "" {
		if u, _, ok := r.BasicAuth(); ok {
			clientID = u
		}
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		client, redirect, challenge, _, expires, err := o.st.TakeCode(ctx, hashToken(r.PostForm.Get("code")))
		if err != nil || expires < time.Now().Unix() || client != clientID || redirect != r.PostForm.Get("redirect_uri") {
			oauthError(w, 400, "invalid_grant", "code is invalid, expired or already used")
			return
		}
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) != 1 {
			oauthError(w, 400, "invalid_grant", "PKCE verification failed")
			return
		}
		o.issue(ctx, w, client)
	case "refresh_token":
		h := hashToken(r.PostForm.Get("refresh_token"))
		client, ok := o.st.CheckToken(ctx, h, "refresh")
		if !ok || client != clientID {
			oauthError(w, 400, "invalid_grant", "refresh token is invalid or expired")
			return
		}
		if deleted, err := o.st.DeleteToken(ctx, h); err != nil || !deleted {
			oauthError(w, 400, "invalid_grant", "refresh token already used")
			return
		}
		o.issue(ctx, w, client)
	default:
		oauthError(w, 400, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

// Protect wraps the MCP handler: OAuth access token or the static API_TOKEN.
func (o *OAuth) Protect(next http.Handler) http.Handler {
	challenge := `Bearer resource_metadata="` + o.cfg.PublicURL + `/.well-known/oauth-protected-resource"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok, found := strings.CutPrefix(auth, "Bearer ")
		if found && tok != "" {
			if o.cfg.APIToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(o.cfg.APIToken)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := o.st.CheckToken(r.Context(), hashToken(tok), "access"); ok {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", challenge)
		oauthError(w, 401, "invalid_token", "missing or invalid access token")
	})
}

// LoginGuard slows down password guessing: after 5 failures in 15 minutes
// every attempt is refused until the window passes. It is global (not per IP)
// because the server sits behind a proxy; the cost is that an attacker can
// lock the owner out for 15 minutes, never in.
type LoginGuard struct {
	mu       sync.Mutex
	failures []time.Time
}

func (g *LoginGuard) Check(given, want string) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cutoff := time.Now().Add(-15 * time.Minute)
	g.failures = slices.DeleteFunc(g.failures, func(t time.Time) bool { return t.Before(cutoff) })
	if len(g.failures) >= 5 {
		return false, "Too many wrong passwords. Try again in 15 minutes."
	}
	a, b := sha256.Sum256([]byte(given)), sha256.Sum256([]byte(want))
	if subtle.ConstantTimeCompare(a[:], b[:]) == 1 {
		return true, ""
	}
	g.failures = append(g.failures, time.Now())
	time.Sleep(500 * time.Millisecond)
	return false, "Wrong password."
}
