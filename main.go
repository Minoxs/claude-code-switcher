package main

import (
	"fmt"
	"os"
	"os/exec"
)

const usage = `ccx: switch between Claude accounts, keeping one shared config.

  ccx list                 saved accounts, with a * on the active one
  ccx current              the account logged in right now
  ccx add [name]           save the current account as a profile
  ccx use <name>           swap to a saved account (takes effect next launch)
  ccx use <name> -r        swap, then run claude --resume in this directory
  ccx use <name> -c        swap, then run claude --continue in this directory
  ccx rm <name>            delete a saved profile

A swap rewrites only the account identity and tokens. Sessions, settings,
skills, plugins and history are shared and left untouched, so a conversation
started on one account resumes on another.`

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
	active := ""
	if live, err := p.captureLive(); err == nil {
		active = accountUUID(live.OAuthAccount)
	}
	for _, prof := range profs {
		mark := " "
		if accountUUID(prof.Identity.OAuthAccount) == active && active != "" {
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
		return fmt.Errorf("usage: ccx use <name> [-r|-c]")
	}
	name, err := profileName(args[0])
	if err != nil {
		return err
	}
	launch := ""
	for _, a := range args[1:] {
		switch a {
		case "-r", "--resume":
			launch = "--resume"
		case "-c", "--continue":
			launch = "--continue"
		default:
			return fmt.Errorf("unknown flag %q", a)
		}
	}

	prof, err := p.loadProfile(name)
	if err != nil {
		return err
	}
	if err := p.apply(prof.Identity); err != nil {
		return err
	}
	fmt.Printf("switched to %q (%s)\n", prof.Name, prof.Email)

	if launch == "" {
		return nil
	}
	claude, err := lookupClaude()
	if err != nil {
		return err
	}
	cmd := exec.Command(claude, launch)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
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

func lookupClaude() (string, error) {
	for _, name := range []string{"claude", "claude.cmd", "claude.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("claude not found on PATH")
}
