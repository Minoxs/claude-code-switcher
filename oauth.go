package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The public OAuth client id Claude Code registers as. Overridable in case
// Anthropic rotates it.
const defaultClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

const tokenEndpoint = "https://claude.ai/v1/oauth/token"

// refreshSkew refreshes a token this long before it actually expires, so an
// in-flight request never carries a token that dies mid-forward.
const refreshSkew = 60 * time.Minute

// oauthCreds mirrors the claudeAiOauth object inside .credentials.json.
type oauthCreds struct {
	AccessToken           string   `json:"accessToken"`
	RefreshToken          string   `json:"refreshToken"`
	ExpiresAt             int64    `json:"expiresAt"` // epoch millis
	RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	SubscriptionType      string   `json:"subscriptionType,omitempty"`
	RateLimitTier         string   `json:"rateLimitTier,omitempty"`
}

func parseCreds(raw json.RawMessage) (oauthCreds, error) {
	var c oauthCreds
	err := json.Unmarshal(raw, &c)
	return c, err
}

func (c oauthCreds) expired() bool {
	return time.Now().Add(refreshSkew).UnixMilli() >= c.ExpiresAt
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"` // seconds
}

// refresh exchanges the rotating refresh token for a fresh access token. The
// response carries a new refresh token that invalidates the old one, so the
// caller must persist the result or the next refresh fails.
func refresh(c oauthCreds, clientID string) (oauthCreds, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
		"client_id":     clientID,
	})
	req, err := http.NewRequest(http.MethodPost, tokenEndpoint, bytes.NewReader(body))
	if err != nil {
		return c, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("token refresh %d: %s", resp.StatusCode, string(data))
	}

	var tr tokenResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return c, fmt.Errorf("decode token response: %w", err)
	}
	c.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		c.RefreshToken = tr.RefreshToken
	}
	c.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).UnixMilli()
	return c, nil
}
