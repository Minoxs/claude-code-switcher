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

// apply writes an identity into the live files, preserving every other key.
func (p paths) apply(id Identity) error {
	cfg, err := readObject(p.configJSON)
	if err != nil {
		return fmt.Errorf("read %s: %w", p.configJSON, err)
	}
	cfg["oauthAccount"] = id.OAuthAccount
	uid, err := json.Marshal(id.UserID)
	if err != nil {
		return err
	}
	cfg["userID"] = uid
	if err := writeObject(p.configJSON, cfg); err != nil {
		return fmt.Errorf("write %s: %w", p.configJSON, err)
	}

	creds, err := readObject(p.credsJSON)
	if err != nil {
		return fmt.Errorf("read %s: %w", p.credsJSON, err)
	}
	creds["claudeAiOauth"] = id.Credentials
	if err := writeObject(p.credsJSON, creds); err != nil {
		return fmt.Errorf("write %s: %w", p.credsJSON, err)
	}
	return nil
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

// writeObject serializes to a temp file in the same directory and renames it
// over the target, so a crash mid-write cannot truncate the live config.
func writeObject(path string, m map[string]json.RawMessage) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ccx-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
