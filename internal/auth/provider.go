// Package auth logs in to brokers and keeps each broker's current access
// token. These tokens can place real orders, so nothing here may print one.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"calm/internal/market"
)

// Secret is a value that masks itself when printed or marshalled. Use Reveal
// only at the point it is sent to a broker or encrypted.
type Secret string

const masked = "[redacted]"

// String masks the secret for every fmt verb that uses Stringer.
func (Secret) String() string { return masked }

// GoString masks the secret for %#v.
func (Secret) GoString() string { return masked }

// MarshalJSON masks the secret.
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(masked) }

// Reveal returns the real value.
func (s Secret) Reveal() string { return string(s) }

// Token is a broker session.
type Token struct {
	Broker      string
	UserID      string
	AccessToken Secret
	IssuedAt    time.Time
	ExpiresAt   time.Time
}

// Valid reports whether the token is usable at t. A 401 from the broker still
// overrides this, since a token can also be revoked early.
func (t Token) Valid(at time.Time) bool { return at.Before(t.ExpiresAt) }

// ValidThrough reports whether the token lasts until at least end. Trading
// uses this to refuse a token that would expire during the session.
func (t Token) ValidThrough(end time.Time) bool { return !end.After(t.ExpiresAt) }

// Provider is one broker's login flow.
type Provider interface {
	Name() string
	// TokenParam names the query parameter holding the value to exchange.
	TokenParam() string
	// LoginURL is the broker page to send the user to.
	LoginURL(state string) string
	// Exchange turns the callback's query parameters into a token.
	Exchange(ctx context.Context, callback url.Values) (Token, error)
	// Revoke ends the session at the broker.
	Revoke(ctx context.Context, t Token) error
}

// nextCutoff returns the first hour:minute IST after issued.
func nextCutoff(issued time.Time, hour, minute int) time.Time {
	c := market.DateOf(issued).At(hour, minute)
	if issued.Before(c) {
		return c
	}
	return c.AddDate(0, 0, 1)
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// send performs a request and returns the status and capped body. Transport
// errors are unwrapped because url.Error prints the full URL, and Zerodha's
// revoke carries the token in its query string.
func send(req *http.Request) (int, []byte, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, fmt.Errorf("reach broker: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("read broker response: %w", err)
	}
	return resp.StatusCode, body, nil
}
