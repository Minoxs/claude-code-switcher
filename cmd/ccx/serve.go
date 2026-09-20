package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultPort = "8787"
const defaultUpstream = "https://api.anthropic.com"

// The OAuth capability Claude Code requests carry. Gateway mode drops it, so
// the proxy re-adds it before forwarding to the subscription backend.
const defaultOAuthBeta = "oauth-2025-04-20"

// usageSnapshot holds the rate-limit headers observed on an account's last
// response, verbatim, so ccx never has to guess Anthropic's claim names.
type usageSnapshot struct {
	ObservedAt time.Time         `json:"observedAt"`
	Model      string            `json:"model,omitempty"`
	Context1M  bool              `json:"context1m,omitempty"`
	Headers    map[string]string `json:"headers"`
}

// manager owns the live account selection and keeps its token fresh. It is the
// sole refresher of every stored account, so refresh-token rotation never
// collides with Claude Code.
type manager struct {
	p        paths
	clientID string

	autostart bool

	mu           sync.Mutex
	active       string
	serving      string
	prof         *Profile
	usage        map[string]usageSnapshot
	limitedUntil map[string]time.Time
	primeAttempt map[string]time.Time

	refreshMu sync.Mutex
}

func (p paths) activeFile() string { return filepath.Join(p.claudeDir, "switcher", "active") }

func (p paths) usageFile() string { return filepath.Join(p.claudeDir, "switcher", "usage.json") }

func (p paths) logFile() string { return filepath.Join(p.claudeDir, "switcher", "proxy.log") }

// setupLog points the standard logger at the switcher log file so a hidden
// proxy leaves a trace even with no console attached. It tees to stderr for the
// foreground run.
func setupLog(p paths) error {
	if err := os.MkdirAll(filepath.Dir(p.logFile()), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p.logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("ccx ")
	return nil
}

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
	m := &manager{
		p:            p,
		clientID:     clientID,
		active:       p.readActive(),
		usage:        map[string]usageSnapshot{},
		limitedUntil: map[string]time.Time{},
		primeAttempt: map[string]time.Time{},
	}
	m.loadUsage()
	return m
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

// candidates lists the accounts to try for a request: the active one first,
// then the rest by name, dropping any still inside a rate-limit cooldown. When
// every account is cooling down it returns the single one that resets soonest,
// so a fully-limited fleet stops rotating and parks there.
func (m *manager) candidates() []string {
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if active == "" {
		return nil
	}
	profs, err := m.p.listProfiles()
	if err != nil {
		return []string{active}
	}

	ordered := []string{active}
	for _, prof := range profs {
		if prof.Name != active {
			ordered = append(ordered, prof.Name)
		}
	}

	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	eligible := make([]string, 0, len(ordered))
	soonest := ""
	var soonestAt time.Time
	for _, name := range ordered {
		until, ok := m.limitedUntil[name]
		if !ok || !until.After(now) {
			eligible = append(eligible, name)
			continue
		}
		if soonest == "" || until.Before(soonestAt) {
			soonest, soonestAt = name, until
		}
	}
	if len(eligible) > 0 {
		return eligible
	}
	return []string{soonest}
}

func (m *manager) activeName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// setServing records the account that last handled a forwarded request, which
// rotation can move off the chosen account without repointing the selection.
func (m *manager) setServing(name string) {
	m.mu.Lock()
	m.serving = name
	m.mu.Unlock()
}

// servingName is the account currently answering requests, falling back to the
// chosen account before any request has been forwarded.
func (m *manager) servingName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.serving != "" {
		return m.serving
	}
	return m.active
}

func (m *manager) markLimited(name string, until time.Time) {
	m.mu.Lock()
	m.limitedUntil[name] = until
	m.mu.Unlock()
}

func (m *manager) clearLimited(name string) {
	m.mu.Lock()
	delete(m.limitedUntil, name)
	m.mu.Unlock()
}

// soonestReset is the earliest instant any account leaves its cooldown, so a
// fully-limited fleet reports the nearest reopening rather than whichever
// account happened to answer last
func (m *manager) soonestReset() (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var soonest time.Time
	for _, until := range m.limitedUntil {
		if soonest.IsZero() || until.Before(soonest) {
			soonest = until
		}
	}
	return soonest, !soonest.IsZero()
}

// loadCreds returns the named profile and its parsed credentials, using the
// cached active profile when it matches.
func (m *manager) loadCreds(name string) (Profile, oauthCreds, error) {
	m.mu.Lock()
	if m.prof != nil && m.active == name {
		prof := *m.prof
		m.mu.Unlock()
		creds, err := parseCreds(prof.Identity.Credentials)
		return prof, creds, err
	}
	m.mu.Unlock()

	prof, err := m.p.loadProfile(name)
	if err != nil {
		return Profile{}, oauthCreds{}, err
	}
	creds, err := parseCreds(prof.Identity.Credentials)
	return prof, creds, err
}

// tokenFor returns a valid access token for the named account, refreshing and
// persisting the rotated credentials when the current one is near expiry.
func (m *manager) tokenFor(name string) (string, error) {
	_, creds, err := m.loadCreds(name)
	if err != nil {
		return "", err
	}
	if !creds.expired() {
		return creds.AccessToken, nil
	}

	// Serialize refreshes so a burst of expired requests rotates the token
	// once, and never hold m.mu across the network call.
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	_, creds, err = m.loadCreds(name)
	if err != nil {
		return "", err
	}
	if !creds.expired() {
		return creds.AccessToken, nil
	}

	fresh, err := refresh(creds, m.clientID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(fresh)
	if err != nil {
		return "", err
	}
	if err := m.persistCreds(name, raw); err != nil {
		return "", err
	}
	return fresh.AccessToken, nil
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
func (m *manager) recordUsage(account, model string, ctx1m bool, h http.Header) {
	snap := usageSnapshot{ObservedAt: time.Now(), Model: model, Context1M: ctx1m, Headers: map[string]string{}}
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
	m.saveUsage()
}

// loadUsage restores the snapshots recorded before the last shutdown so ccx
// usage is not blank until fresh traffic arrives.
func (m *manager) loadUsage() {
	data, err := os.ReadFile(m.p.usageFile())
	if err != nil {
		return
	}
	var saved map[string]usageSnapshot
	if err := json.Unmarshal(data, &saved); err != nil || saved == nil {
		return
	}
	m.mu.Lock()
	m.usage = saved
	m.mu.Unlock()
}

// saveUsage writes the current snapshots through to disk. It runs after every
// recorded response because the proxy is terminated, not asked to shut down.
func (m *manager) saveUsage() {
	m.mu.Lock()
	data, err := json.Marshal(m.usage)
	m.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.p.usageFile()), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(m.p.usageFile(), data, 0o600)
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
	autostart := os.Getenv("CCX_AUTOSTART") == "1" || strings.EqualFold(os.Getenv("CCX_AUTOSTART"), "true")
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				port = args[i+1]
				i++
			}
		case "--hidden":
			hidden = true
		case "--autostart":
			autostart = true
		}
	}
	if hidden {
		hideConsole()
	}
	if err := setupLog(p); err != nil {
		fmt.Fprintln(os.Stderr, "ccx: log: "+err.Error())
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
	mgr.autostart = autostart
	if name := mgr.pickSoonest(time.Now()); name != "" && name != mgr.activeName() {
		if err := mgr.switchTo(name); err != nil {
			log.Printf("startup pick %s: %v", name, err)
		} else {
			log.Printf("startup: serving %s, its window resets soonest", name)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ccx/", func(w http.ResponseWriter, r *http.Request) {
		handleControl(mgr, upstream, beta, w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mgr.forward(upstream, beta, w, r)
	})

	addr := "127.0.0.1:" + port
	fmt.Printf("ccx proxy on http://%s  -> %s\n", addr, upstreamStr)
	fmt.Printf("active account: %s\n", mgr.status())
	fmt.Printf("point Claude Code at it:\n  ANTHROPIC_BASE_URL=http://%s\n  ANTHROPIC_AUTH_TOKEN=ccx-proxy\n", addr)

	if autostart {
		if profs, err := mgr.p.listProfiles(); err == nil && len(profs) > 0 {
			fmt.Printf("autostart: priming idle accounts, offset %s\n", shortDur(primeWindow/time.Duration(len(profs))))
		}
		go mgr.runAutostart(upstream, beta)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// forward proxies one request, rotating to the next saved account and replaying
// when an account is rate-limited, so a 429 stays hidden from Claude Code while
// another account still has budget. It makes at most one attempt per candidate,
// and when every account is limited it parks on the soonest to reset and hands
// the 429 back rather than looping.
func (m *manager) forward(upstream *url.URL, beta string, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "ccx: read request: "+err.Error(), http.StatusBadGateway)
		return
	}

	names := m.candidates()
	if len(names) == 0 {
		http.Error(w, "ccx: no active account; run: ccx use <name>", http.StatusBadGateway)
		return
	}

	model := requestModel(body)
	ctx1m := strings.Contains(r.Header.Get("anthropic-beta"), "context-1m")

	for i, name := range names {
		last := i == len(names)-1

		token, err := m.tokenFor(name)
		if err != nil {
			log.Printf("account %s: token: %v", name, err)
			if last {
				http.Error(w, "ccx: "+err.Error(), http.StatusBadGateway)
				return
			}
			continue
		}

		resp, err := http.DefaultTransport.RoundTrip(buildUpstream(r, body, upstream, token, beta))
		if err != nil {
			log.Printf("account %s: upstream: %v", name, err)
			if last {
				http.Error(w, "ccx: upstream: "+err.Error(), http.StatusBadGateway)
				return
			}
			continue
		}
		m.recordUsage(name, model, ctx1m, resp.Header)

		if resp.StatusCode == http.StatusTooManyRequests {
			m.markLimited(name, resetAfter(resp))
			if !last {
				resp.Body.Close()
				continue
			}
			// Every account is limited. A streaming request gets the mid-stream
			// limit shape (200 with the unified headers, then a rate-limit error
			// event), which Claude Code surfaces with the reset and can auto-
			// continue, unlike the flat upfront 429 it renders as a dead error.
			m.setServing(name)
			if requestStream(body) {
				m.serveLimited(upstream, beta, w, r, body, model, ctx1m, resp)
				resp.Body.Close()
				return
			}
			if reset, ok := m.soonestReset(); ok {
				rewriteLimitHeaders(resp, reset)
				log.Printf("all accounts limited; soonest reset %s", reset.Format(time.RFC3339))
			}
			streamResponse(w, resp)
			resp.Body.Close()
			return
		}

		m.clearLimited(name)
		m.setServing(name)
		streamResponse(w, resp)
		resp.Body.Close()
		return
	}
}

// tryForward makes one pass over the current candidates and returns the first
// response that is not a 429, recording usage and cooldowns as it goes. The
// bool is false when every candidate is still limited or unreachable.
func (m *manager) tryForward(upstream *url.URL, beta string, r *http.Request, body []byte, model string, ctx1m bool) (*http.Response, bool) {
	for _, name := range m.candidates() {
		token, err := m.tokenFor(name)
		if err != nil {
			log.Printf("account %s: token: %v", name, err)
			continue
		}
		resp, err := http.DefaultTransport.RoundTrip(buildUpstream(r, body, upstream, token, beta))
		if err != nil {
			log.Printf("account %s: upstream: %v", name, err)
			continue
		}
		m.recordUsage(name, model, ctx1m, resp.Header)
		if resp.StatusCode == http.StatusTooManyRequests {
			m.markLimited(name, resetAfter(resp))
			resp.Body.Close()
			continue
		}
		m.clearLimited(name)
		m.setServing(name)
		return resp, true
	}
	return nil, false
}

// limitPing is how often serveLimited writes an SSE keepalive while holding a
// request open, so the client's idle timeout does not fire during the wait.
const limitPing = 10 * time.Second

// maxHold bounds how long serveLimited keeps a request open waiting for a
// window to reopen. A longer wait would trip the client's own request timeout,
// so past it the proxy emits the rate-limit error and lets Claude Code schedule
// its retry instead of holding a doomed connection.
const maxHold = 5 * time.Minute

// serveLimited answers a streaming request when every account is limited. It
// commits to a 200 event-stream carrying the unified rate-limit headers, the
// shape Claude Code reads as a mid-stream limit. When the soonest window is
// within maxHold it holds the stream alive and forwards for real once it
// reopens; otherwise it emits a rate-limit error event carrying the reset.
func (m *manager) serveLimited(upstream *url.URL, beta string, w http.ResponseWriter, r *http.Request, body []byte, model string, ctx1m bool, limit *http.Response) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		if reset, ok := m.soonestReset(); ok {
			rewriteLimitHeaders(limit, reset)
		}
		streamResponse(w, limit)
		return
	}

	reset, ok := m.soonestReset()
	if !ok {
		reset = time.Now().Add(time.Hour)
	}

	dst := w.Header()
	for key, vals := range limit.Header {
		if strings.HasPrefix(strings.ToLower(key), "anthropic-ratelimit-") {
			dst[key] = append([]string(nil), vals...)
		}
	}
	epoch := strconv.FormatInt(reset.Unix(), 10)
	dst.Set("anthropic-ratelimit-unified-reset", epoch)
	dst.Set("anthropic-ratelimit-unified-5h-reset", epoch)
	dst.Set("anthropic-ratelimit-unified-status", "rejected")
	dst.Set("Content-Type", "text/event-stream")
	dst.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	log.Printf("all accounts limited; soonest reset %s", reset.Format(time.RFC3339))

	ctx := r.Context()
	deadline := time.Now().Add(maxHold)
	ping := time.NewTicker(limitPing)
	defer ping.Stop()
	for {
		if !time.Now().Before(reset) {
			if resp, served := m.tryForward(upstream, beta, r, body, model, ctx1m); served {
				if resp.StatusCode == http.StatusOK {
					pipeStream(w, resp, flusher)
				} else {
					relayStreamError(w, flusher, resp)
				}
				resp.Body.Close()
				return
			}
			if next, ok := m.soonestReset(); ok {
				reset = next
			}
		}
		if reset.After(deadline) {
			writeRateLimitEvent(w, flusher, reset)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// pipeStream copies an already-headered upstream body to the client, flushing
// each chunk so a held stream resumes token by token once served.
func pipeStream(w io.Writer, resp *http.Response, flusher http.Flusher) {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if rerr != nil {
			return
		}
	}
}

// writeRateLimitEvent ends a held stream with an Anthropic-shaped rate-limit
// error naming the reset, so Claude Code surfaces when usage returns.
func writeRateLimitEvent(w io.Writer, flusher http.Flusher, reset time.Time) {
	msg := fmt.Sprintf("All accounts are rate-limited; usage resets %s", humanReset(reset, time.Now()))
	data := fmt.Sprintf(`{"type":"error","error":{"type":"rate_limit_error","message":%q}}`, msg)
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
	flusher.Flush()
}

// relayStreamError forwards a non-429 upstream failure as an error event, since
// the response was already committed to an event-stream before the retry ran.
func relayStreamError(w io.Writer, flusher http.Flusher, resp *http.Response) {
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", strings.TrimSpace(string(payload)))
	flusher.Flush()
}

// buildUpstream rewrites an inbound request for the subscription backend: it
// swaps in the account's bearer token, re-adds the OAuth beta, and replays the
// buffered body so the request can be retried against another account.
func buildUpstream(r *http.Request, body []byte, upstream *url.URL, token, beta string) *http.Request {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = upstream.Scheme
	out.URL.Host = upstream.Host
	out.Host = upstream.Host
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))

	out.Header.Del("X-Api-Key")
	out.Header.Set("Authorization", "Bearer "+token)
	out.Header.Set("anthropic-beta", mergeBeta(out.Header.Get("anthropic-beta"), beta))
	if out.Header.Get("x-app") == "" {
		out.Header.Set("x-app", "cli")
	}
	for key := range out.Header {
		if isHopHeader(key) {
			out.Header.Del(key)
		}
	}
	return out
}

// streamResponse copies an upstream response back to the client, flushing every
// chunk so SSE tokens arrive as they are produced instead of being buffered.
func streamResponse(w http.ResponseWriter, resp *http.Response) {
	dst := w.Header()
	for key, vals := range resp.Header {
		if isHopHeader(key) {
			continue
		}
		dst[key] = append([]string(nil), vals...)
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

func isHopHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Proxy-Connection", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// setRetryAfter overwrites Retry-After with the seconds until reset, so a
// handed-back 429 points the client at the nearest reopening across the fleet
// rather than the account that answered
func setRetryAfter(resp *http.Response, reset time.Time) {
	secs := int(time.Until(reset).Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}
	resp.Header.Set("Retry-After", strconv.Itoa(secs))
}

// rewriteLimitHeaders collapses a handed-back 429 onto one fleet-wide window.
// Claude Code re-derives its rate-limit state from the unified headers per
// response, so pointing the reset at the soonest account arms its auto-continue
// timer for the moment the proxy regains capacity. Only the 5h and aggregate
// reset move; the weekly window is left as the serving account reported it.
func rewriteLimitHeaders(resp *http.Response, reset time.Time) {
	epoch := strconv.FormatInt(reset.Unix(), 10)
	resp.Header.Set("anthropic-ratelimit-unified-reset", epoch)
	resp.Header.Set("anthropic-ratelimit-unified-5h-reset", epoch)
	resp.Header.Set("anthropic-ratelimit-unified-status", "rejected")
	setRetryAfter(resp, reset)
}

// resetAfter reads when a rate-limited account will accept requests again,
// preferring Retry-After and falling back to the unified reset headers. It
// defaults to an hour when the response says nothing.
func resetAfter(resp *http.Response) time.Time {
	now := time.Now()
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return now.Add(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(ra); err == nil {
			return t
		}
	}
	for _, key := range []string{
		"anthropic-ratelimit-unified-reset",
		"anthropic-ratelimit-unified-5h-reset",
		"anthropic-ratelimit-unified-7d-reset",
	} {
		if t, ok := parseReset(resp.Header.Get(key), now); ok {
			return t
		}
	}
	return now.Add(time.Hour)
}

func handleControl(mgr *manager, upstream *url.URL, beta string, w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ccx/refresh":
		mgr.refreshAll(upstream, beta)
		handleUsage(mgr, w)
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

// refreshAll re-pings the accounts whose usage window is already open so ccx
// usage reflects live limits. It leaves a closed window alone: priming one would
// force it open and collapse the autostart stagger.
func (m *manager) refreshAll(upstream *url.URL, beta string) {
	profs, err := m.p.listProfiles()
	if err != nil {
		return
	}
	now := time.Now()
	for _, prof := range profs {
		if !m.windowRunning(prof.Name, now) {
			continue
		}
		if err := m.prime(upstream, beta, prof.Name); err != nil {
			log.Printf("refresh %s: %v", prof.Name, err)
		}
	}
}

func handleUsage(mgr *manager, w http.ResponseWriter) {
	profs, err := mgr.p.listProfiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	out := map[string]any{}
	serving := mgr.servingName()
	for _, prof := range profs {
		entry := map[string]any{"email": prof.Email, "active": prof.Name == serving}
		if snap, ok := mgr.usageSnapshot(prof.Name); ok {
			entry["observedAt"] = snap.ObservedAt
			entry["model"] = snap.Model
			entry["context1m"] = snap.Context1M
			entry["headers"] = snap.Headers
		}
		if t, ok := mgr.nextPrime(prof.Name, now); ok {
			entry["nextPrime"] = t
		}
		out[prof.Name] = entry
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// requestModel pulls the model an inbound request names, so ccx usage can show
// what Claude Code actually put on the wire rather than what its picker claims.
func requestModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Model
}

// requestStream reports whether an inbound request asked for a streamed reply,
// so the proxy only holds and re-frames requests it can keep alive.
func requestStream(body []byte) bool {
	var v struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Stream
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
