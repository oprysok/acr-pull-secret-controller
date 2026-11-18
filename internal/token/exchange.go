package token

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// exchange trades a Microsoft Entra access token for an ACR refresh token.
// See https://github.com/Azure/acr/blob/main/docs/AAD-OAuth.md
func exchange(ctx context.Context, c *http.Client, registry, tenantID, accessToken string) (string, error) {
	form := url.Values{
		"grant_type":   {"access_token"},
		"service":      {registry},
		"tenant":       {tenantID},
		"access_token": {accessToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+registry+"/oauth2/exchange", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange with %s: %w", registry, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exchange with %s: HTTP %d: %s", registry, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("exchange with %s: decode response: %w", registry, err)
	}
	if out.RefreshToken == "" {
		return "", fmt.Errorf("exchange with %s: response has no refresh_token", registry)
	}
	return out.RefreshToken, nil
}

// expiry reads the exp claim of a JWT without verifying it. The registry is
// the party that verifies the token; we only need to know when to renew it.
func expiry(jwt string, fallback time.Time) time.Time {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fallback
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return fallback
	}
	return time.Unix(claims.Exp, 0)
}
