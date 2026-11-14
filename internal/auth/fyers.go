package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Fyers is the Fyers API v3 login. Tokens expire daily at 06:30 IST.
type Fyers struct {
	AppID       string // e.g. ABCD1234-100
	Secret      Secret
	RedirectURI string
	BaseURL     string // default https://api-t1.fyers.in/api/v3
	Now         func() time.Time
}

// Name returns the broker's name.
func (f *Fyers) Name() string { return "fyers" }

// TokenParam names the redirect's auth code parameter.
func (f *Fyers) TokenParam() string { return "auth_code" }

func (f *Fyers) base() string {
	if f.BaseURL != "" {
		return f.BaseURL
	}
	return "https://api-t1.fyers.in/api/v3"
}

// LoginURL returns the Fyers login page.
func (f *Fyers) LoginURL(state string) string {
	q := url.Values{"client_id": {f.AppID}, "redirect_uri": {f.RedirectURI},
		"response_type": {"code"}, "state": {state}}
	return f.base() + "/generate-authcode?" + q.Encode()
}

// appIDHash is the SHA-256 of "appId:secret" that validate-authcode expects.
func appIDHash(appID string, secret Secret) string {
	sum := sha256.Sum256([]byte(appID + ":" + secret.Reveal()))
	return hex.EncodeToString(sum[:])
}

// Exchange trades the auth code for an access token.
func (f *Fyers) Exchange(ctx context.Context, cb url.Values) (Token, error) {
	code := cb.Get("auth_code")
	if code == "" {
		return Token{}, errors.New("login was not completed")
	}
	body, err := json.Marshal(map[string]string{"grant_type": "authorization_code",
		"appIdHash": appIDHash(f.AppID, f.Secret), "code": code})
	if err != nil {
		return Token{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base()+"/validate-authcode", bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, resp, err := send(req)
	if err != nil {
		return Token{}, err
	}
	var r struct {
		S           string `json:"s"`
		Code        int    `json:"code"`
		Message     string `json:"message"`
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(resp, &r)
	if status != http.StatusOK || r.S != "ok" {
		return Token{}, fmt.Errorf("fyers refused the login: %s", fyersError(status, r.Code, r.Message))
	}
	if r.AccessToken == "" {
		return Token{}, errors.New("fyers returned no access token")
	}
	now := now(f.Now)
	return Token{Broker: f.Name(), UserID: f.AppID, AccessToken: Secret(r.AccessToken),
		IssuedAt: now, ExpiresAt: nextCutoff(now, 6, 30)}, nil
}

// Revoke ends the API session.
func (f *Fyers) Revoke(ctx context.Context, t Token) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base()+"/logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", f.AppID+":"+t.AccessToken.Reveal())
	status, body, err := send(req)
	if err != nil {
		return err
	}
	var r struct {
		S       string `json:"s"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &r)
	if status != http.StatusOK || r.S != "ok" {
		return fmt.Errorf("fyers refused the logout: %s", fyersError(status, r.Code, r.Message))
	}
	return nil
}

func fyersError(status, code int, msg string) string {
	if msg != "" {
		return fmt.Sprintf("%s (%d)", msg, code)
	}
	return fmt.Sprintf("HTTP %d", status)
}
