package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const taskName = "ccx-proxy"

// cmdInstall registers the proxy to start at logon via a Scheduled Task, then
// starts it. The task runs `ccx serve --hidden` so no console window appears.
func cmdInstall() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("ccx install currently supports Windows only")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	action := fmt.Sprintf(`"%s" serve --hidden`, exe)
	if err := schtasks("/Create", "/TN", taskName, "/TR", action,
		"/SC", "ONLOGON", "/RL", "LIMITED", "/F"); err != nil {
		return err
	}
	if err := schtasks("/Run", "/TN", taskName); err != nil {
		return fmt.Errorf("task registered but failed to start now: %w", err)
	}
	fmt.Printf("installed %q, starts at logon and is running now\n", taskName)
	return nil
}

func cmdUninstall() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("ccx uninstall currently supports Windows only")
	}
	if err := schtasks("/Delete", "/TN", taskName, "/F"); err != nil {
		return err
	}
	fmt.Printf("removed %q\n", taskName)
	return nil
}

func schtasks(args ...string) error {
	cmd := exec.Command("schtasks", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
