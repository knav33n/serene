package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Breeze is ICICI Direct's API, which project_calm downloads MCX history
// with. Its login hands back an apisession in the redirect URL, and that is
// the day's token: there's no exchange and no state, so it's pasted.
type Breeze struct {
	APIKey  string
	BaseURL string // https://api.icicidirect.com by default
	Now     func() time.Time
}

func (b *Breeze) base() string {
	if b.BaseURL != "" {
		return b.BaseURL
	}
	return "https://api.icicidirect.com"
}

func (b *Breeze) Name() string       { return "breeze" }
func (b *Breeze) TokenParam() string { return "apisession" }

// LoginURL ignores state: Breeze doesn't send it back.
func (b *Breeze) LoginURL(string) string {
	return b.base() + "/apiuser/login?api_key=" + url.QueryEscape(b.APIKey)
}

// breezeErrors are customerdetails' errors, as its SDK words them.
var breezeErrors = map[string]string{"Invalid session.": "the apisession is wrong",
	"Public Key does not exist.": "BREEZE_API_KEY is wrong", "Resource not available.": "the apisession has expired"}

// Exchange checks the apisession with Breeze's customerdetails and keeps it
// until 06:00 IST, when project_calm counts a day's logins from.
func (b *Breeze) Exchange(ctx context.Context, q url.Values) (Token, error) {
	session := strings.TrimSpace(q.Get("apisession"))
	if session == "" {
		return Token{}, errors.New("no apisession")
	}
	body, _ := json.Marshal(map[string]string{"SessionToken": session, "AppKey": b.APIKey})
	// A GET with a JSON body, as Breeze expects
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base()+"/breezeapi/api/v1/customerdetails", bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, resp, err := send(req)
	if err != nil {
		return Token{}, err
	}
	var d struct {
		Success *struct {
			SessionToken string `json:"session_token"`
		} `json:"Success"`
		Error any `json:"Error"`
	}
	if err := json.Unmarshal(resp, &d); err != nil {
		return Token{}, fmt.Errorf("breeze login: HTTP %d", status)
	}
	if d.Success == nil {
		msg := fmt.Sprint(d.Error)
		if m, ok := breezeErrors[msg]; ok {
			msg = m
		}
		return Token{}, fmt.Errorf("breeze login failed: %s", msg)
	}
	raw, err := base64.StdEncoding.DecodeString(d.Success.SessionToken)
	if err != nil {
		return Token{}, errors.New("breeze login: its session token isn't base64")
	}
	user, _, _ := strings.Cut(string(raw), ":")
	at := now(b.Now)
	return Token{Broker: "breeze", UserID: user, AccessToken: Secret(session), IssuedAt: at, ExpiresAt: nextCutoff(at, 6, 0)}, nil
}

// Revoke does nothing: Breeze has no logout, and its session ends by itself.
func (b *Breeze) Revoke(context.Context, Token) error { return nil }
