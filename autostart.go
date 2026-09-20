package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"time"
)

// primeWindow is the length of the subscription usage window autostart keeps
// alive on each account. Anthropic resets the unified limit five hours after a
// window's first request.
const primeWindow = 5 * time.Hour

// autostartRetryGap bounds how often a single account is primed, so an account
// whose prime keeps failing is retried on a slow cadence instead of every tick.
const autostartRetryGap = 10 * time.Minute

// primeBody is a minimal Claude Code request. Subscription OAuth rejects a
// request whose first system block is not the Claude Code identifier, so the
// prime carries it verbatim and asks for a one-word reply.
const primeBody = `{"model":"claude-haiku-4-5","max_tokens":5,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[{"role":"user","content":"emit hi and stop"}]}`

// runAutostart keeps every account's usage window running and staggered. The
// active account anchors the schedule at its real window start, and each other
// account is offset by window/N so their resets spread evenly across the window.
func (m *manager) runAutostart(upstream *url.URL, beta string) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	m.autostartTick(upstream, beta)
	for range tick.C {
		m.autostartTick(upstream, beta)
	}
}

func (m *manager) autostartTick(upstream *url.URL, beta string) {
	profs, err := m.p.listProfiles()
	if err != nil || len(profs) == 0 {
		return
	}
	order := autostartOrder(m.activeName(), profs)
	offset := primeWindow / time.Duration(len(order))

	now := time.Now()
	ref := m.referenceStart(order[0], now)
	pos := now.Sub(ref) % primeWindow
	for i, name := range order {
		phase := time.Duration(i) * offset
		if pos < phase || pos >= phase+offset {
			continue
		}
		if m.windowRunning(name, now) {
			continue
		}
		if !m.shouldPrime(name, now) {
			continue
		}
		_ = m.prime(upstream, beta, name)
	}
}

// referenceStart is the instant the schedule offsets from: the active account's
// current window start when one is running, otherwise now so the active account
// primes immediately and the rest fall in behind it.
func (m *manager) referenceStart(active string, now time.Time) time.Time {
	if reset, ok := m.windowReset(active, now); ok && reset.After(now) {
		return reset.Add(-primeWindow)
	}
	return now
}

// windowRunning reports whether the account's usage window is still open, read
// from the reset header its last response carried.
func (m *manager) windowRunning(name string, now time.Time) bool {
	reset, ok := m.windowReset(name, now)
	return ok && reset.After(now)
}

func (m *manager) windowReset(name string, now time.Time) (time.Time, bool) {
	snap, ok := m.usageSnapshot(name)
	if !ok {
		return time.Time{}, false
	}
	return parseReset(snap.Headers["anthropic-ratelimit-unified-5h-reset"], now)
}

// shouldPrime records an attempt and reports whether the retry gap has elapsed,
// so a failing prime does not repeat every tick.
func (m *manager) shouldPrime(name string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.primeAttempt[name]; ok && now.Sub(last) < autostartRetryGap {
		return false
	}
	m.primeAttempt[name] = now
	return true
}

// prime sends one minimal request on the account to open its usage window,
// recording the rate-limit headers it comes back with.
func (m *manager) prime(upstream *url.URL, beta, name string) error {
	token, err := m.tokenFor(name)
	if err != nil {
		return err
	}
	body := []byte(primeBody)
	req, err := http.NewRequest(http.MethodPost, upstream.String()+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultTransport.RoundTrip(buildUpstream(req, body, upstream, token, beta))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	m.recordUsage(name, requestModel(body), false, resp.Header)
	if resp.StatusCode == http.StatusTooManyRequests {
		m.markLimited(name, resetAfter(resp))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// nextPrime is the instant autostart will next consider priming the account,
// the start of its stagger slot within the current window cycle. It reports
// false when autostart is off, so callers can tell "not scheduled" from "due".
func (m *manager) nextPrime(name string, now time.Time) (time.Time, bool) {
	if !m.autostart {
		return time.Time{}, false
	}
	profs, err := m.p.listProfiles()
	if err != nil || len(profs) == 0 {
		return time.Time{}, false
	}
	order := autostartOrder(m.activeName(), profs)
	idx := indexOf(order, name)
	if idx < 0 {
		return time.Time{}, false
	}
	offset := primeWindow / time.Duration(len(order))
	ref := m.referenceStart(order[0], now)
	pos := now.Sub(ref) % primeWindow
	slot := time.Duration(idx) * offset

	switch {
	case pos < slot:
		return now.Add(slot - pos), true
	case pos < slot+offset:
		return now, true
	default:
		return now.Add(primeWindow - pos + slot), true
	}
}

func indexOf(names []string, name string) int {
	for i, n := range names {
		if n == name {
			return i
		}
	}
	return -1
}

// autostartOrder puts the active account first, then the rest by name, so the
// active account anchors the stagger and the others fill the window behind it.
func autostartOrder(active string, profs []Profile) []string {
	names := make([]string, 0, len(profs))
	if active != "" {
		names = append(names, active)
	}
	for _, prof := range profs {
		if prof.Name != active {
			names = append(names, prof.Name)
		}
	}
	return names
}
