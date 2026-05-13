// Package auth handles Tailscale auth — either a static auth key (CLI/env)
// or OAuth2 client credentials that mint an ephemeral, pre-approved auth key.
//
// Code is ported from upstream tailsocks/auth.go with minimal changes.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const userAgent = "flotilla/1"

// OAuth2Credentials represents the OAuth2 client credentials.
//
//nolint:tagliatelle
type OAuth2Credentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Tag          string `json:"tag"`

	tagEncoded string
}

// GetAuthToken obtains an OAuth2 access token then mints a short-lived
// Tailscale auth key tagged with c.Tag.
func (c *OAuth2Credentials) GetAuthToken(ctx context.Context, ephemeral bool) (string, error) {
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get OAuth2 access token: %w", err)
	}

	authKey, err := c.CreateAuthKey(ctx, accessToken, ephemeral)
	if err != nil {
		return "", fmt.Errorf("failed to create Tailscale auth key: %w", err)
	}

	return authKey, nil
}

func (c *OAuth2Credentials) prepareTag() error {
	tag := strings.TrimSpace(c.Tag)
	if tag == "" {
		return errors.New("tag is required in credentials file")
	}

	if !strings.HasPrefix(tag, "tag:") {
		tag = "tag:" + tag
	}

	tagEnc, err := json.Marshal(tag)
	if err != nil {
		return fmt.Errorf("failed to encode tag as JSON: %w", err)
	}

	c.tagEncoded = string(tagEnc)
	return nil
}

func (c *OAuth2Credentials) getAccessToken(parentCtx context.Context) (string, error) {
	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", c.ClientID)
	data.Set("client_secret", c.ClientSecret)

	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tailscale.com/api/v2/oauth/token", strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	if res.StatusCode != http.StatusOK {
		//nolint:tagliatelle
		var errRes struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		err = json.NewDecoder(res.Body).Decode(&errRes)
		if err == nil && errRes.Error != "" {
			return "", fmt.Errorf("OAuth2 error: %s - %s", errRes.Error, errRes.ErrorDescription)
		}
		return "", fmt.Errorf("unexpected status code: %d", res.StatusCode)
	}

	//nolint:tagliatelle
	var tokenRes struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	err = json.NewDecoder(res.Body).Decode(&tokenRes)
	if err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if tokenRes.AccessToken == "" {
		return "", errors.New("empty access token in response")
	}

	slog.Debug("Obtained OAuth2 access token", "expires_in", tokenRes.ExpiresIn)
	return tokenRes.AccessToken, nil
}

// CreateAuthKey turns a Tailscale OAuth2 access token into a single-use,
// pre-authorized, tagged Tailscale auth key.
func (c *OAuth2Credentials) CreateAuthKey(parentCtx context.Context, accessToken string, ephemeral bool) (string, error) {
	if c.tagEncoded == "" {
		err := c.prepareTag()
		if err != nil {
			return "", err
		}
	}

	const reqBodyFmt = `{"capabilities": {"devices": {"create": {"reusable": false, "ephemeral": %v, "preauthorized": true, "tags": [%s]}}}, "expirySeconds": 300}`
	reqBody := fmt.Sprintf(reqBodyFmt, ephemeral, c.tagEncoded)

	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tailscale.com/api/v2/tailnet/-/keys", strings.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	if res.StatusCode != http.StatusOK {
		var errRes struct {
			Message string `json:"message"`
		}
		err = json.NewDecoder(res.Body).Decode(&errRes)
		if err == nil && errRes.Message != "" {
			return "", fmt.Errorf("API error: %s", errRes.Message)
		}
		return "", fmt.Errorf("unexpected status code: %d", res.StatusCode)
	}

	var keyRes struct {
		Key string `json:"key"`
	}
	err = json.NewDecoder(res.Body).Decode(&keyRes)
	if err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if keyRes.Key == "" {
		return "", errors.New("empty auth key in response")
	}

	slog.Debug("Obtained Tailscale auth key")
	return keyRes.Key, nil
}

// DefaultCredentialsPath is the canonical on-disk location for OAuth2 creds.
func DefaultCredentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".config", "flotilla", "oauth2.json"), nil
}

// LoadOAuth2Credentials reads + validates an OAuth2 credentials JSON file.
func LoadOAuth2Credentials(path string) (*OAuth2Credentials, error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("OAuth2 credentials file '%s' does not exist", path)
	} else if err != nil {
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var creds OAuth2Credentials
	err = json.Unmarshal(data, &creds)
	if err != nil {
		return nil, fmt.Errorf("failed to parse credentials file: %w", err)
	}

	if creds.ClientID == "" {
		return nil, errors.New("client_id is required in credentials file")
	}
	if creds.ClientSecret == "" {
		return nil, errors.New("client_secret is required in credentials file")
	}
	if creds.Tag == "" {
		return nil, errors.New("tag is required in credentials file")
	}
	err = creds.prepareTag()
	if err != nil {
		return nil, err
	}

	return &creds, nil
}

// AuthKeyFromEnv reads TS_AUTHKEY or TS_AUTH_KEY from the environment.
func AuthKeyFromEnv() string {
	authKey := strings.TrimSpace(os.Getenv("TS_AUTHKEY"))
	if authKey != "" {
		slog.Info("Using auth key from environment TS_AUTHKEY")
		return authKey
	}

	authKey = strings.TrimSpace(os.Getenv("TS_AUTH_KEY"))
	if authKey != "" {
		slog.Info("Using auth key from environment TS_AUTH_KEY")
		return authKey
	}

	return ""
}

// OAuthAccessTokenFromEnv returns a pre-issued OAuth2 access token, if one
// has been provided via TS_OAUTH_ACCESS_TOKEN. Useful in CI / federated flows.
func OAuthAccessTokenFromEnv() (token, tag string) {
	token = strings.TrimSpace(os.Getenv("TS_OAUTH_ACCESS_TOKEN"))
	tag = strings.TrimSpace(os.Getenv("TS_OAUTH_TAG"))
	return token, tag
}
