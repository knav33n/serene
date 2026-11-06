package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Zerodha is the Kite Connect login. Tokens expire at 06:00 IST.
type Zerodha struct {
	APIKey    string
	APISecret Secret
	LoginBase string // default https://kite.zerodha.com
	APIBase   string // default https://api.kite.trade
	Now       func() time.Time
}

// Name returns the broker's name.
func (z *Zerodha) Name() string { return "zerodha" }

// TokenParam names the redirect's request token parameter.
func (z *Zerodha) TokenParam() string { return "request_token" }

func (z *Zerodha) api() string {
	if z.APIBase != "" {
		return z.APIBase
	}
	return "https://api.kite.trade"
}

// LoginURL returns the Kite login page. Kite has no state parameter, but
// redirect_params values are sent back to the redirect URL.
func (z *Zerodha) LoginURL(state string) string {
	base := z.LoginBase
	if base == "" {
		base = "https://kite.zerodha.com"
	}
	q := url.Values{"v": {"3"}, "api_key": {z.APIKey},
		"redirect_params": {url.Values{"state": {state}}.Encode()}}
	return base + "/connect/login?" + q.Encode()
}

func checksum(apiKey, requestToken string, secret Secret) string {
	sum := sha256.Sum256([]byte(apiKey + requestToken + secret.Reveal()))
	return hex.EncodeToString(sum[:])
}

// Exchange trades the short-lived request token for an access token.
func (z *Zerodha) Exchange(ctx context.Context, cb url.Values) (Token, error) {
	rt := cb.Get("request_token")
	if rt == "" {
		return Token{}, errors.New("login was not completed")
	}
	form := url.Values{"api_key": {z.APIKey}, "request_token": {rt}, "checksum": {checksum(z.APIKey, rt, z.APISecret)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, z.api()+"/session/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Kite-Version", "3")
	status, body, err := send(req)
	if err != nil {
		return Token{}, err
	}
	if status != http.StatusOK {
		return Token{}, fmt.Errorf("zerodha refused the login: %s", kiteError(status, body))
	}
	var r struct {
		Data struct {
			AccessToken string `json:"access_token"`
			UserID      string `json:"user_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Data.AccessToken == "" {
		return Token{}, errors.New("zerodha returned no access token")
	}
	now := now(z.Now)
	return Token{Broker: z.Name(), UserID: r.Data.UserID, AccessToken: Secret(r.Data.AccessToken),
		IssuedAt: now, ExpiresAt: nextCutoff(now, 6, 0)}, nil
}

// Revoke ends the API session. It does not log out of Kite web or mobile.
func (z *Zerodha) Revoke(ctx context.Context, t Token) error {
	q := url.Values{"api_key": {z.APIKey}, "access_token": {t.AccessToken.Reveal()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, z.api()+"/session/token?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Kite-Version", "3")
	status, body, err := send(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("zerodha refused the logout: %s", kiteError(status, body))
	}
	return nil
}

func kiteError(status int, body []byte) string {
	var r struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &r) == nil && r.Message != "" {
		return r.Message
	}
	return fmt.Sprintf("HTTP %d", status)
}
