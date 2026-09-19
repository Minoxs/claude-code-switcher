package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Profile struct {
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	SavedAt  string   `json:"savedAt"`
	Identity Identity `json:"identity"`
}

func profileName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("empty profile name")
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) {
		return "", fmt.Errorf("profile name %q has illegal characters", name)
	}
	return name, nil
}

func (p paths) profilePath(name string) string {
	return filepath.Join(p.profilesDir, name+".json")
}

func (p paths) saveProfile(prof Profile) error {
	if err := os.MkdirAll(p.profilesDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(prof, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.profilePath(prof.Name), data, 0o600)
}

func (p paths) loadProfile(name string) (Profile, error) {
	data, err := os.ReadFile(p.profilePath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return Profile{}, fmt.Errorf("no profile named %q", name)
		}
		return Profile{}, err
	}
	var prof Profile
	if err := json.Unmarshal(data, &prof); err != nil {
		return Profile{}, err
	}
	return prof, nil
}

func (p paths) listProfiles() ([]Profile, error) {
	entries, err := os.ReadDir(p.profilesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Profile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		prof, err := p.loadProfile(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, prof)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func newProfile(name string, id Identity) Profile {
	return Profile{
		Name:     name,
		Email:    accountEmail(id.OAuthAccount),
		SavedAt:  time.Now().UTC().Format(time.RFC3339),
		Identity: id,
	}
}
