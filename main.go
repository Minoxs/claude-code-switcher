package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

const usage = `ccx: switch Claude accounts inside a live session, via a local proxy.

  ccx add [name]           save the currently logged-in account as a profile
  ccx list                 saved accounts, with a * on the active one
  ccx current              the account logged in on disk right now
  ccx use <name>           make <name> active; a running proxy switches instantly
  ccx rm <name>            delete a saved profile
  ccx serve [--port N]     run the proxy Claude Code talks to
  ccx usage                per-account rate-limit usage the proxy has observed
  ccx env [bash]           print the env vars that point Claude Code at the proxy
  ccx ping [-v]            exit 0 if the proxy is up, non-zero otherwise
  ccx install              start the proxy at logon (Windows scheduled task)
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
		return cmdUsage()
	case "ping":
		return cmdPing(args[1:])
	case "install":
		return cmdInstall()
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

	if reply, ok := controlSwitch(name); ok {
		fmt.Print(reply)
		return nil
	}
	fmt.Printf("selected %q (%s). start ccx serve to activate it\n", prof.Name, prof.Email)
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

func cmdUsage() error {
	port := defaultPort
	if v := os.Getenv("CCX_PORT"); v != "" {
		port = v
	}
	resp, err := http.Get("http://127.0.0.1:" + port + "/ccx/usage")
	if err != nil {
		return fmt.Errorf("no proxy on port %s; start it with ccx serve", port)
	}
	defer resp.Body.Close()

	var accounts map[string]struct {
		Email      string            `json:"email"`
		ObservedAt string            `json:"observedAt"`
		Headers    map[string]string `json:"headers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&accounts); err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("no saved accounts. run: ccx add <name>")
		return nil
	}

	names := make([]string, 0, len(accounts))
	for name := range accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := accounts[name]
		fmt.Printf("%s  %s\n", name, a.Email)
		if len(a.Headers) == 0 {
			fmt.Println("    no requests seen yet")
			continue
		}
		hkeys := make([]string, 0, len(a.Headers))
		for k := range a.Headers {
			hkeys = append(hkeys, k)
		}
		sort.Strings(hkeys)
		for _, k := range hkeys {
			fmt.Printf("    %-48s %s\n", strings.TrimPrefix(k, "anthropic-ratelimit-"), a.Headers[k])
		}
		fmt.Printf("    (as of %s)\n", a.ObservedAt)
	}
	return nil
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
		return nil
	}
	fmt.Printf("$env:ANTHROPIC_BASE_URL = \"http://127.0.0.1:%s\"\n", port)
	fmt.Printf("$env:ANTHROPIC_AUTH_TOKEN = \"ccx-proxy\"\n")
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
