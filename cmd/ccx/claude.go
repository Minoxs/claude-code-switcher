package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Identity is the per-account slice of Claude's config. Everything else in
// .claude.json and .credentials.json is shared and never touched by a swap.
type Identity struct {
	OAuthAccount json.RawMessage `json:"oauthAccount"`
	UserID       string          `json:"userID"`
	Credentials  json.RawMessage `json:"credentials"`
}

type paths struct {
	configJSON  string // ~/.claude.json
	claudeDir   string // ~/.claude
	credsJSON   string // ~/.claude/.credentials.json
	profilesDir string // ~/.claude/switcher/profiles
}

func resolvePaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, err
	}
	dir := filepath.Join(home, ".claude")
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		dir = v
	}
	return paths{
		configJSON:  filepath.Join(home, ".claude.json"),
		claudeDir:   dir,
		credsJSON:   filepath.Join(dir, ".credentials.json"),
		profilesDir: filepath.Join(dir, "switcher", "profiles"),
	}, nil
}

// captureLive reads the account currently logged in on disk.
func (p paths) captureLive() (Identity, error) {
	cfg, err := readObject(p.configJSON)
	if err != nil {
		return Identity{}, fmt.Errorf("read %s: %w", p.configJSON, err)
	}
	oauth, ok := cfg["oauthAccount"]
	if !ok {
		return Identity{}, fmt.Errorf("no oauthAccount in %s; log in first", p.configJSON)
	}
	var userID string
	if raw, ok := cfg["userID"]; ok {
		_ = json.Unmarshal(raw, &userID)
	}

	creds, err := readObject(p.credsJSON)
	if err != nil {
		return Identity{}, fmt.Errorf("read %s: %w", p.credsJSON, err)
	}
	blob, ok := creds["claudeAiOauth"]
	if !ok {
		return Identity{}, fmt.Errorf("no claudeAiOauth in %s; log in first", p.credsJSON)
	}

	return Identity{OAuthAccount: oauth, UserID: userID, Credentials: blob}, nil
}

// applyLive writes prof's account into the on-disk config and credentials, so a
// Claude Code session talking straight to Anthropic, not the proxy, runs as this
// account. It is the inverse of captureLive.
func (p paths) applyLive(prof Profile) error {
	cfg, err := readObject(p.configJSON)
	if err != nil {
		return fmt.Errorf("read %s: %w", p.configJSON, err)
	}
	cfg["oauthAccount"] = prof.Identity.OAuthAccount
	if prof.Identity.UserID != "" {
		uid, err := json.Marshal(prof.Identity.UserID)
		if err != nil {
			return err
		}
		cfg["userID"] = uid
	}
	if err := writeObject(p.configJSON, cfg); err != nil {
		return err
	}

	creds, err := readObject(p.credsJSON)
	if err != nil {
		return fmt.Errorf("read %s: %w", p.credsJSON, err)
	}
	creds["claudeAiOauth"] = prof.Identity.Credentials
	return writeObject(p.credsJSON, creds)
}

// adoptLive copies the live login's credentials into the profile for the same
// account when the live blob is the newer of the two. Claude Code talking
// straight to Anthropic refreshes the live account and rotates its single-use
// refresh token, leaving the profile holding a dead one. It returns the name of
// the profile it updated, or "" when there was nothing to adopt.
func (p paths) adoptLive() (string, error) {
	live, err := p.captureLive()
	if err != nil {
		return "", nil
	}
	liveCreds, err := parseCreds(live.Credentials)
	if err != nil {
		return "", nil
	}
	uuid := accountUUID(live.OAuthAccount)
	if uuid == "" {
		return "", nil
	}
	profs, err := p.listProfiles()
	if err != nil {
		return "", err
	}
	for _, prof := range profs {
		if accountUUID(prof.Identity.OAuthAccount) != uuid {
			continue
		}
		stored, err := parseCreds(prof.Identity.Credentials)
		if err != nil {
			return "", nil
		}
		if liveCreds.RefreshToken == stored.RefreshToken {
			return "", nil
		}
		if liveCreds.ExpiresAt <= stored.ExpiresAt {
			return "", nil
		}
		prof.Identity.Credentials = live.Credentials
		if err := p.saveProfile(prof); err != nil {
			return "", err
		}
		return prof.Name, nil
	}
	return "", nil
}

// writeLiveCreds hands rotated credentials to the live login when it is the
// same account, so a Claude Code session reading the file does not refresh with
// the token the proxy just consumed
func (p paths) writeLiveCreds(uuid string, raw json.RawMessage) error {
	if uuid == "" {
		return nil
	}
	cfg, err := readObject(p.configJSON)
	if err != nil {
		return err
	}
	if accountUUID(cfg["oauthAccount"]) != uuid {
		return nil
	}
	creds, err := readObject(p.credsJSON)
	if err != nil {
		return err
	}
	creds["claudeAiOauth"] = raw
	return writeObject(p.credsJSON, creds)
}

// writeObject serializes m and swaps it into place with a rename, so a crash
// mid-write cannot leave ~/.claude.json truncated.
func writeObject(path string, m map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".ccx.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// accountUUID pulls the stable id out of an oauthAccount blob for matching.
func accountUUID(oauth json.RawMessage) string {
	var v struct {
		AccountUUID string `json:"accountUuid"`
	}
	_ = json.Unmarshal(oauth, &v)
	return v.AccountUUID
}

func accountEmail(oauth json.RawMessage) string {
	var v struct {
		Email string `json:"emailAddress"`
	}
	_ = json.Unmarshal(oauth, &v)
	return v.Email
}

func readObject(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

