package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultPort = "8787"
const defaultUpstream = "https://api.anthropic.com"

// The OAuth capability Claude Code requests carry. Gateway mode drops it, so
// the proxy re-adds it before forwarding to the subscription backend.
const defaultOAuthBeta = "oauth-2025-04-20"

type ctxKey int

const (
	tokenKey   ctxKey = 0
	accountKey ctxKey = 1
)

// usageSnapshot holds the rate-limit headers observed on an account's last
// response, verbatim, so ccx never has to guess Anthropic's claim names.
type usageSnapshot struct {
	ObservedAt time.Time         `json:"observedAt"`
	Headers    map[string]string `json:"headers"`
}

// manager owns the live account selection and keeps its token fresh. It is the
// sole refresher of every stored account, so refresh-token rotation never
// collides with Claude Code.
type manager struct {
	p        paths
	clientID string

	mu     sync.Mutex
	active string
	prof   *Profile
	usage  map[string]usageSnapshot

	refreshMu sync.Mutex
}

func (p paths) activeFile() string { return filepath.Join(p.claudeDir, "switcher", "active") }

func (p paths) readActive() string {
	data, err := os.ReadFile(p.activeFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (p paths) writeActive(name string) error {
	if err := os.MkdirAll(filepath.Dir(p.activeFile()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.activeFile(), []byte(name+"\n"), 0o600)
}

func newManager(p paths) *manager {
	clientID := defaultClientID
	if v := os.Getenv("CCX_OAUTH_CLIENT_ID"); v != "" {
		clientID = v
	}
	return &manager{p: p, clientID: clientID, active: p.readActive(), usage: map[string]usageSnapshot{}}
}

// switchTo makes name the account every subsequent request uses.
func (m *manager) switchTo(name string) error {
	prof, err := m.p.loadProfile(name)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.active = name
	m.prof = &prof
	m.mu.Unlock()
	return m.p.writeActive(name)
}

// token returns a valid access token for the active account and its name,
// refreshing and persisting the rotated credentials when the current one is
// near expiry.
func (m *manager) token() (string, string, error) {
	m.mu.Lock()
	name, creds, err := m.currentCredsLocked()
	m.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	if !creds.expired() {
		return creds.AccessToken, name, nil
	}

	// Serialize refreshes so a burst of expired requests rotates the token
	// once, and never hold m.mu across the network call.
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	m.mu.Lock()
	name, creds, err = m.currentCredsLocked()
	m.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	if !creds.expired() {
		return creds.AccessToken, name, nil
	}

	fresh, err := refresh(creds, m.clientID)
	if err != nil {
		return "", "", err
	}
	raw, err := json.Marshal(fresh)
	if err != nil {
		return "", "", err
	}
	if err := m.persistCreds(name, raw); err != nil {
		return "", "", err
	}
	return fresh.AccessToken, name, nil
}

// currentCredsLocked resolves the active account and its parsed credentials,
// loading the profile from disk the first time. Caller holds m.mu.
func (m *manager) currentCredsLocked() (string, oauthCreds, error) {
	if m.prof == nil {
		if m.active == "" {
			return "", oauthCreds{}, fmt.Errorf("no active account; run: ccx use <name>")
		}
		prof, err := m.p.loadProfile(m.active)
		if err != nil {
			return "", oauthCreds{}, err
		}
		m.prof = &prof
	}
	creds, err := parseCreds(m.prof.Identity.Credentials)
	return m.active, creds, err
}

// persistCreds writes rotated credentials for name to disk. It saves by name
// even if the active account changed mid-refresh, so a rotated refresh token
// is never dropped once the old one is invalidated upstream.
func (m *manager) persistCreds(name string, raw json.RawMessage) error {
	m.mu.Lock()
	if m.prof != nil && m.active == name {
		m.prof.Identity.Credentials = raw
		prof := *m.prof
		m.mu.Unlock()
		return m.p.saveProfile(prof)
	}
	m.mu.Unlock()

	prof, err := m.p.loadProfile(name)
	if err != nil {
		return err
	}
	prof.Identity.Credentials = raw
	return m.p.saveProfile(prof)
}

// recordUsage snapshots the unified rate-limit headers from a forwarded
// response against the account that made the request.
func (m *manager) recordUsage(account string, h http.Header) {
	snap := usageSnapshot{ObservedAt: time.Now(), Headers: map[string]string{}}
	for key, vals := range h {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "anthropic-ratelimit-") {
			snap.Headers[lower] = strings.Join(vals, ", ")
		}
	}
	if len(snap.Headers) == 0 {
		return
	}
	m.mu.Lock()
	m.usage[account] = snap
	m.mu.Unlock()
}

func (m *manager) usageSnapshot(account string) (usageSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap, ok := m.usage[account]
	return snap, ok
}

func (m *manager) status() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == "" {
		return "(none)"
	}
	email := ""
	if m.prof != nil {
		email = m.prof.Email
	}
	return fmt.Sprintf("%s %s", m.active, email)
}

func cmdServe(p paths, args []string) error {
	port := defaultPort
	if v := os.Getenv("CCX_PORT"); v != "" {
		port = v
	}
	hidden := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				port = args[i+1]
				i++
			}
		case "--hidden":
			hidden = true
		}
	}
	if hidden {
		hideConsole()
	}

	upstreamStr := defaultUpstream
	if v := os.Getenv("CCX_UPSTREAM"); v != "" {
		upstreamStr = v
	}
	upstream, err := url.Parse(upstreamStr)
	if err != nil {
		return err
	}
	beta := defaultOAuthBeta
	if v := os.Getenv("CCX_OAUTH_BETA"); v != "" {
		beta = v
	}

	mgr := newManager(p)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Set("Authorization", "Bearer "+pr.In.Context().Value(tokenKey).(string))
			pr.Out.Header.Set("anthropic-beta", mergeBeta(pr.Out.Header.Get("anthropic-beta"), beta))
			if pr.Out.Header.Get("x-app") == "" {
				pr.Out.Header.Set("x-app", "cli")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if account, ok := resp.Request.Context().Value(accountKey).(string); ok {
				mgr.recordUsage(account, resp.Header)
			}
			return nil
		},
		FlushInterval: -1, // stream SSE token-by-token instead of buffering
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ccx/", func(w http.ResponseWriter, r *http.Request) {
		handleControl(mgr, w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		token, account, err := mgr.token()
		if err != nil {
			http.Error(w, "ccx: "+err.Error(), http.StatusBadGateway)
			return
		}
		ctx := context.WithValue(r.Context(), tokenKey, token)
		ctx = context.WithValue(ctx, accountKey, account)
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})

	addr := "127.0.0.1:" + port
	fmt.Printf("ccx proxy on http://%s  -> %s\n", addr, upstreamStr)
	fmt.Printf("active account: %s\n", mgr.status())
	fmt.Printf("point Claude Code at it:\n  ANTHROPIC_BASE_URL=http://%s\n  ANTHROPIC_AUTH_TOKEN=ccx-proxy\n", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

func handleControl(mgr *manager, w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ccx/switch":
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}
		if err := mgr.switchTo(name); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "switched to %s\n", mgr.status())
	case "/ccx/status":
		fmt.Fprintln(w, mgr.status())
	case "/ccx/usage":
		handleUsage(mgr, w)
	default:
		http.NotFound(w, r)
	}
}

func handleUsage(mgr *manager, w http.ResponseWriter) {
	profs, err := mgr.p.listProfiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{}
	for _, prof := range profs {
		entry := map[string]any{"email": prof.Email}
		if snap, ok := mgr.usageSnapshot(prof.Name); ok {
			entry["observedAt"] = snap.ObservedAt
			entry["headers"] = snap.Headers
		}
		out[prof.Name] = entry
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func mergeBeta(existing, add string) string {
	if add == "" {
		return existing
	}
	for _, part := range strings.Split(existing, ",") {
		if strings.TrimSpace(part) == add {
			return existing
		}
	}
	if existing == "" {
		return add
	}
	return existing + "," + add
}
