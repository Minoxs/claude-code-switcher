package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const usage = `ccx: switch Claude accounts inside a live session, via a local proxy.

  ccx add [name]           save the currently logged-in account as a profile
  ccx list                 saved accounts, with a * on the active one
  ccx current              the account logged in on disk right now
  ccx use <name>           swap the on-disk login to <name>; a running proxy also flips live
  ccx rm <name>            delete a saved profile
  ccx serve [--port N]     run the proxy; add --autostart to stagger account windows
  ccx usage                per-account rate-limit usage the proxy has observed
  ccx usage --refresh      ping every account first, then show live usage
  ccx env [bash]           print the env vars that point Claude Code at the proxy
  ccx ping [-v]            exit 0 if the proxy is up, non-zero otherwise
  ccx install              start the proxy at logon (Windows scheduled task)
  ccx restart              restart the installed proxy on the current binary
  ccx uninstall            remove the logon task

Load accounts by logging into each one in Claude Code, then ccx add <name>.
Run ccx serve, point Claude Code at it with ccx env, and ccx use flips the
active account for the next request of the live session, no restart.`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ccx: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Println(usage)
		return nil
	}
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		return cmdList(p)
	case "current", "who":
		return cmdCurrent(p)
	case "add", "save":
		return cmdAdd(p, args[1:])
	case "use", "switch":
		return cmdUse(p, args[1:])
	case "rm", "remove", "delete":
		return cmdRemove(p, args[1:])
	case "serve", "proxy":
		return cmdServe(p, args[1:])
	case "env":
		return cmdEnv(args[1:])
	case "usage":
		return cmdUsage(p, args[1:])
	case "ping":
		return cmdPing(args[1:])
	case "install":
		return cmdInstall()
	case "restart":
		return cmdRestart()
	case "uninstall":
		return cmdUninstall()
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q (try: ccx help)", args[0])
	}
}

func cmdList(p paths) error {
	profs, err := p.listProfiles()
	if err != nil {
		return err
	}
	if len(profs) == 0 {
		fmt.Println("no saved accounts. run: ccx add <name>")
		return nil
	}
	active := p.readActive()
	for _, prof := range profs {
		mark := " "
		if prof.Name == active && active != "" {
			mark = "*"
		}
		fmt.Printf("%s %-16s %s\n", mark, prof.Name, prof.Email)
	}
	return nil
}

func cmdCurrent(p paths) error {
	live, err := p.captureLive()
	if err != nil {
		return err
	}
	name := "(unsaved)"
	if profs, err := p.listProfiles(); err == nil {
		for _, prof := range profs {
			if accountUUID(prof.Identity.OAuthAccount) == accountUUID(live.OAuthAccount) {
				name = prof.Name
				break
			}
		}
	}
	fmt.Printf("%s  %s\n", accountEmail(live.OAuthAccount), name)
	return nil
}

func cmdAdd(p paths, args []string) error {
	live, err := p.captureLive()
	if err != nil {
		return err
	}
	name := accountEmail(live.OAuthAccount)
	if len(args) > 0 {
		name = args[0]
	}
	name, err = profileName(name)
	if err != nil {
		return err
	}
	if err := p.saveProfile(newProfile(name, live)); err != nil {
		return err
	}
	fmt.Printf("saved %q (%s)\n", name, accountEmail(live.OAuthAccount))
	return nil
}

func cmdUse(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccx use <name>")
	}
	name, err := profileName(args[0])
	if err != nil {
		return err
	}
	prof, err := p.loadProfile(name)
	if err != nil {
		return err
	}
	if err := p.writeActive(name); err != nil {
		return err
	}
	if err := p.applyLive(prof); err != nil {
		return err
	}

	if reply, ok := controlSwitch(name); ok {
		fmt.Print(reply)
		return nil
	}
	fmt.Printf("switched to %q (%s)\n", prof.Name, prof.Email)
	return nil
}

// controlSwitch asks a running proxy to flip the active account. Returns false
// when no proxy is listening.
func controlSwitch(name string) (string, bool) {
	port := defaultPort
	if v := os.Getenv("CCX_PORT"); v != "" {
		port = v
	}
	endpoint := "http://127.0.0.1:" + port + "/ccx/switch?name=" + url.QueryEscape(name)
	resp, err := http.Get(endpoint)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode == http.StatusOK
}

type usageAccount struct {
	Email      string            `json:"email"`
	Active     bool              `json:"active"`
	ObservedAt string            `json:"observedAt"`
	Model      string            `json:"model"`
	Context1M  bool              `json:"context1m"`
	Headers    map[string]string `json:"headers"`
	NextPrime  string            `json:"nextPrime"`
}

func cmdUsage(p paths, args []string) error {
	port := defaultPort
	if v := os.Getenv("CCX_PORT"); v != "" {
		port = v
	}
	refresh := false
	for _, a := range args {
		if a == "--refresh" || a == "-r" {
			refresh = true
		}
	}

	accounts, err := fetchUsage(port, refresh)
	if err != nil {
		if refresh {
			return fmt.Errorf("no proxy on port %s; refresh needs it running", port)
		}
		if accounts, err = readUsageDisk(p); err != nil {
			return err
		}
		fmt.Println("proxy not running; showing last saved usage")
	}
	if len(accounts) == 0 {
		fmt.Println("no saved accounts. run: ccx add <name>")
		return nil
	}
	renderUsage(accounts)
	return nil
}

func fetchUsage(port string, refresh bool) (map[string]usageAccount, error) {
	path := "/ccx/usage"
	if refresh {
		path = "/ccx/refresh"
	}
	resp, err := http.Get("http://127.0.0.1:" + port + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var accounts map[string]usageAccount
	if err := json.NewDecoder(resp.Body).Decode(&accounts); err != nil {
		return nil, err
	}
	return accounts, nil
}

// readUsageDisk builds the same account view from the persisted snapshots, so
// ccx usage still reports the last-known limits when the proxy is not running.
func readUsageDisk(p paths) (map[string]usageAccount, error) {
	profs, err := p.listProfiles()
	if err != nil {
		return nil, err
	}
	snaps := map[string]usageSnapshot{}
	if data, err := os.ReadFile(p.usageFile()); err == nil {
		_ = json.Unmarshal(data, &snaps)
	}
	var liveUUID string
	if live, err := p.captureLive(); err == nil {
		liveUUID = accountUUID(live.OAuthAccount)
	}
	out := map[string]usageAccount{}
	for _, prof := range profs {
		acc := usageAccount{Email: prof.Email, Active: liveUUID != "" && accountUUID(prof.Identity.OAuthAccount) == liveUUID}
		if snap, ok := snaps[prof.Name]; ok {
			acc.ObservedAt = snap.ObservedAt.Format(time.RFC3339Nano)
			acc.Model = snap.Model
			acc.Context1M = snap.Context1M
			acc.Headers = snap.Headers
		}
		out[prof.Name] = acc
	}
	return out, nil
}

func renderUsage(accounts map[string]usageAccount) {
	names := make([]string, 0, len(accounts))
	for name := range accounts {
		names = append(names, name)
	}
	sort.Strings(names)

	now := time.Now()
	for _, name := range names {
		a := accounts[name]
		mark := ""
		if a.Active {
			mark = " (in use)"
		}
		fmt.Printf("%s  %s%s\n", name, a.Email, mark)
		if a.Model != "" {
			ctx := ""
			if a.Context1M {
				ctx = " (1M)"
			}
			fmt.Printf("    %-24s %s%s\n", "model", a.Model, ctx)
		}
		if len(a.Headers) == 0 {
			fmt.Println("    no requests seen yet")
		} else {
			hkeys := make([]string, 0, len(a.Headers))
			for k := range a.Headers {
				hkeys = append(hkeys, k)
			}
			sort.Strings(hkeys)
			for _, k := range hkeys {
				label := strings.TrimPrefix(k, "anthropic-ratelimit-")
				value := a.Headers[k]
				if strings.HasSuffix(k, "reset") {
					if t, ok := parseReset(value, now); ok {
						value = humanReset(t, now)
					}
				}
				fmt.Printf("    %-24s %s\n", label, value)
			}
		}
		if t, err := parseObserved(a.ObservedAt); err == nil {
			fmt.Printf("    seen %s ago\n", shortAgo(now.Sub(t)))
		}
		if t, err := parseObserved(a.NextPrime); err == nil {
			fmt.Printf("    %-24s %s\n", "next prime", humanReset(t, now))
		}
	}
}

// parseReset reads a rate-limit reset value as an RFC3339 timestamp, epoch
// seconds or millis, or a plain seconds-from-now count, so both header shapes
// Anthropic uses land on the same instant.
func parseReset(v string, now time.Time) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		switch {
		case n > 1_000_000_000_000:
			return time.UnixMilli(n), true
		case n > 1_000_000_000:
			return time.Unix(n, 0), true
		default:
			return now.Add(time.Duration(n) * time.Second), true
		}
	}
	return time.Time{}, false
}

func humanReset(t, now time.Time) string {
	local := t.Local().Format("Mon 15:04 MST")
	d := t.Sub(now)
	if d <= 0 {
		return local + " (now)"
	}
	return local + " (in " + shortDur(d) + ")"
}

func parseObserved(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, v)
}

func shortDur(d time.Duration) string {
	d = d.Round(time.Minute)
	h := d / time.Hour
	mnt := (d % time.Hour) / time.Minute
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, mnt)
	}
	return fmt.Sprintf("%dm", mnt)
}

func shortAgo(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return shortDur(d)
}

func cmdPing(args []string) error {
	verbose := false
	for _, a := range args {
		if a == "-v" {
			verbose = true
		}
	}
	port := defaultPort
	if v := os.Getenv("CCX_PORT"); v != "" {
		port = v
	}

	fail := func(err error) {
		if verbose {
			fmt.Fprintln(os.Stderr, "ccx: "+err.Error())
		}
		os.Exit(1)
	}
	resp, err := http.Get("http://127.0.0.1:" + port + "/ccx/status")
	if err != nil {
		fail(fmt.Errorf("no proxy on port %s", port))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("proxy on port %s returned %d", port, resp.StatusCode))
	}
	if verbose {
		fmt.Printf("proxy up on port %s\n", port)
	}
	return nil
}

func cmdEnv(args []string) error {
	port := defaultPort
	if v := os.Getenv("CCX_PORT"); v != "" {
		port = v
	}
	if len(args) > 0 && args[0] == "bash" {
		fmt.Printf("export ANTHROPIC_BASE_URL=\"http://127.0.0.1:%s\"\n", port)
		fmt.Printf("export ANTHROPIC_AUTH_TOKEN=\"ccx-proxy\"\n")
		fmt.Printf("export _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=\"1\"\n")
		fmt.Printf("export ANTHROPIC_BETAS=\"context-1m-2025-08-07\"\n")
		return nil
	}
	fmt.Printf("$env:ANTHROPIC_BASE_URL = \"http://127.0.0.1:%s\"\n", port)
	fmt.Printf("$env:ANTHROPIC_AUTH_TOKEN = \"ccx-proxy\"\n")
	fmt.Printf("$env:_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL = \"1\"\n")
	fmt.Printf("$env:ANTHROPIC_BETAS = \"context-1m-2025-08-07\"\n")
	return nil
}

func cmdRemove(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccx rm <name>")
	}
	name, err := profileName(args[0])
	if err != nil {
		return err
	}
	if err := os.Remove(p.profilePath(name)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no profile named %q", name)
		}
		return err
	}
	fmt.Printf("removed %q\n", name)
	return nil
}
