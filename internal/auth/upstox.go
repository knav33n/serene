package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Upstox is the Upstox OAuth login. Tokens expire at 03:30 IST.
type Upstox struct {
	APIKey      string
	APISecret   Secret
	RedirectURI string
	BaseURL     string // default https://api.upstox.com
	Now         func() time.Time
}

// Name returns the broker's name.
func (u *Upstox) Name() string { return "upstox" }

// TokenParam names the redirect's code parameter.
func (u *Upstox) TokenParam() string { return "code" }

func (u *Upstox) base() string {
	if u.BaseURL != "" {
		return u.BaseURL
	}
	return "https://api.upstox.com"
}

// LoginURL returns the Upstox login page.
func (u *Upstox) LoginURL(state string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {u.APIKey},
		"redirect_uri": {u.RedirectURI}, "state": {state}}
	return u.base() + "/v2/login/authorization/dialog?" + q.Encode()
}

// Exchange trades the single-use code for a token.
func (u *Upstox) Exchange(ctx context.Context, cb url.Values) (Token, error) {
	code := cb.Get("code")
	if code == "" {
		return Token{}, errors.New("login was not completed")
	}
	form := url.Values{"code": {code}, "client_id": {u.APIKey}, "client_secret": {u.APISecret.Reveal()},
		"redirect_uri": {u.RedirectURI}, "grant_type": {"authorization_code"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		u.base()+"/v2/login/authorization/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, body, err := send(req)
	if err != nil {
		return Token{}, err
	}
	if status != http.StatusOK {
		return Token{}, fmt.Errorf("upstox refused the login: %s", upstoxError(status, body))
	}
	var r struct {
		AccessToken string `json:"access_token"`
		UserID      string `json:"user_id"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.AccessToken == "" {
		return Token{}, errors.New("upstox returned no access token")
	}
	now := now(u.Now)
	return Token{Broker: u.Name(), UserID: r.UserID, AccessToken: Secret(r.AccessToken),
		IssuedAt: now, ExpiresAt: nextCutoff(now, 3, 30)}, nil
}

// Revoke ends the session.
func (u *Upstox) Revoke(ctx context.Context, t Token) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.base()+"/v2/logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.AccessToken.Reveal())
	req.Header.Set("Accept", "application/json")
	status, body, err := send(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("upstox refused the logout: %s", upstoxError(status, body))
	}
	return nil
}

func upstoxError(status int, body []byte) string {
	var r struct {
		Errors []struct {
			Code    string `json:"errorCode"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &r) == nil && len(r.Errors) > 0 {
		return fmt.Sprintf("%s (%s)", r.Errors[0].Message, r.Errors[0].Code)
	}
	return fmt.Sprintf("HTTP %d", status)
}

func now(f func() time.Time) time.Time {
	if f != nil {
		return f()
	}
	return time.Now()
}
