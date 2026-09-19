package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const taskName = "ccx-proxy"

// cmdInstall registers the proxy to start at logon via a Scheduled Task, then
// starts it. The task runs `ccx serve --hidden` so no console window appears.
func cmdInstall() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("ccx install currently supports Windows only")
	}
	if err := registerTask(); err != nil {
		return err
	}
	if err := schtasks("/Run", "/TN", taskName); err != nil {
		return fmt.Errorf("task registered but failed to start now: %w", err)
	}
	fmt.Printf("installed %q, starts at logon and is running now\n", taskName)
	return nil
}

// registerTask points the logon task at the current binary, creating or
// overwriting it. Running it again after replacing the exe repoints the task.
func registerTask() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	action := fmt.Sprintf(`"%s" serve --hidden`, exe)
	return schtasks("/Create", "/TN", taskName, "/TR", action,
		"/SC", "ONLOGON", "/RL", "LIMITED", "/F")
}

// cmdRestart repoints the logon task at the current binary, stops the running
// proxy, and starts it again, so a rebuilt exe takes over without a logout.
func cmdRestart() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("ccx restart currently supports Windows only")
	}
	if err := registerTask(); err != nil {
		return err
	}
	port := defaultPort
	if v := os.Getenv("CCX_PORT"); v != "" {
		port = v
	}

	_ = schtasks("/End", "/TN", taskName)
	waitProxy(port, false, 3*time.Second)

	if err := schtasks("/Run", "/TN", taskName); err != nil {
		return err
	}
	if !waitProxy(port, true, 5*time.Second) {
		return fmt.Errorf("task started but proxy did not come up on port %s", port)
	}
	fmt.Printf("restarted %q on the current binary\n", taskName)
	return nil
}

// waitProxy polls /ccx/status until the proxy's reachability matches up, or the
// timeout elapses, returning whether the wanted state was reached.
func waitProxy(port string, up bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get("http://127.0.0.1:" + port + "/ccx/status")
		if err == nil {
			resp.Body.Close()
		}
		if (err == nil) == up {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
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
