package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// burnState names an account the proxy spends first until its weekly window
// resets. Until stays zero while that reset is unknown and the proxy pins it
// from the first response the account answers.
type burnState struct {
	Name  string    `json:"name"`
	Until time.Time `json:"until,omitzero"`
}

func (b burnState) live(now time.Time) bool {
	return b.Until.IsZero() || b.Until.After(now)
}

func (p paths) burnFile() string { return filepath.Join(p.claudeDir, "switcher", "burn.json") }

func (p paths) readBurn() (burnState, bool) {
	data, err := os.ReadFile(p.burnFile())
	if err != nil {
		return burnState{}, false
	}
	var b burnState
	if err := json.Unmarshal(data, &b); err != nil {
		return burnState{}, false
	}
	return b, b.Name != ""
}

func (p paths) writeBurn(b burnState) error {
	if err := os.MkdirAll(filepath.Dir(p.burnFile()), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return os.WriteFile(p.burnFile(), data, 0o600)
}

// weekReset reads the account's upcoming weekly reset from a snapshot. A reset
// already in the past belongs to a finished week and says nothing about the
// current one.
func weekReset(snap usageSnapshot, now time.Time) (time.Time, bool) {
	reset, ok := parseReset(snap.Headers["anthropic-ratelimit-unified-7d-reset"], now)
	if !ok || !reset.After(now) {
		return time.Time{}, false
	}
	return reset, true
}

// burning is the account to spend first, or "" when none is set or its week
// has reset.
func (m *manager) burning(now time.Time) string {
	b, ok := m.p.readBurn()
	if !ok || !b.live(now) {
		return ""
	}
	return b.Name
}

// pinBurn fixes the burn's end at the weekly reset the account just reported.
// Pinning once matters: the header rolls forward to next week the moment this
// one resets, so re-reading it would keep the burn alive forever.
func (m *manager) pinBurn(account string, snap usageSnapshot) {
	b, ok := m.p.readBurn()
	if !ok || b.Name != account || !b.Until.IsZero() {
		return
	}
	reset, ok := weekReset(snap, snap.ObservedAt)
	if !ok {
		return
	}
	b.Until = reset
	if err := m.p.writeBurn(b); err != nil {
		log.Printf("burn %s: %v", account, err)
		return
	}
	log.Printf("burn: %s until %s", account, reset.Format(time.RFC3339))
}

func cmdBurn(p paths, args []string) error {
	now := time.Now()
	if len(args) == 0 {
		b, ok := p.readBurn()
		if !ok || !b.live(now) {
			fmt.Println("not burning")
			return nil
		}
		fmt.Println(burnLine(b, now))
		return nil
	}
	if args[0] == "--off" {
		if err := os.Remove(p.burnFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Println("not burning")
		return nil
	}

	name, err := profileName(args[0])
	if err != nil {
		return err
	}
	if _, err := p.loadProfile(name); err != nil {
		return err
	}
	snaps := map[string]usageSnapshot{}
	if data, err := os.ReadFile(p.usageFile()); err == nil {
		_ = json.Unmarshal(data, &snaps)
	}
	b := burnState{Name: name}
	b.Until, _ = weekReset(snaps[name], now)
	if err := p.writeBurn(b); err != nil {
		return err
	}
	fmt.Println(burnLine(b, now))
	return nil
}

func burnLine(b burnState, now time.Time) string {
	if b.Until.IsZero() {
		return fmt.Sprintf("burning %s until its weekly reset", b.Name)
	}
	return fmt.Sprintf("burning %s until %s", b.Name, humanReset(b.Until, now))
}
