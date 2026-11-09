package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

var shellAuthEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_BETAS",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL",
}

// cmdLogin repairs a saved profile whose token went stale. The sign-in runs in a
// throwaway config dir so the profile is the only holder of the new refresh
// token, which is single-use
func cmdLogin(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccx login <name>")
	}
	name, err := profileName(args[0])
	if err != nil {
		return err
	}
	prof, err := p.loadProfile(name)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	claude, err := findClaude(home)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "ccx-login-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	cmd := exec.Command(claude, loginArgs(prof)...)
	cmd.Env = append(signInEnv(), "CLAUDE_CONFIG_DIR="+dir)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude auth login: %w", err)
	}

	signedIn := paths{
		configJSON: filepath.Join(dir, ".claude.json"),
		credsJSON:  filepath.Join(dir, ".credentials.json"),
	}
	live, err := signedIn.captureLive()
	if err != nil {
		return err
	}
	if accountUUID(live.OAuthAccount) != accountUUID(prof.Identity.OAuthAccount) {
		return fmt.Errorf("signed in as %s, but %q is %s; profile unchanged", accountEmail(live.OAuthAccount), name, prof.Email)
	}
	prof.Identity.Credentials = live.Credentials
	if err := p.saveProfile(prof); err != nil {
		return err
	}
	fmt.Printf("repaired %q (%s)\n", name, prof.Email)
	return nil
}

// signInEnv drops auth set by the calling shell, which often points at the proxy
// or carries another account's token and would outrank the login being made
func signInEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return slices.Contains(shellAuthEnv, strings.ToUpper(key))
	})
}

// findClaude also checks the native installer's location, which shells started
// outside a login profile may not have on PATH
func findClaude(home string) (string, error) {
	for _, candidate := range []string{"claude", filepath.Join(home, ".local", "bin", "claude")} {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("claude not found on PATH or in ~/.local/bin")
}

func loginArgs(prof Profile) []string {
	args := []string{"auth", "login"}
	if prof.Email == "" {
		return args
	}
	return append(args, "--email", prof.Email)
}
