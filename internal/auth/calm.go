package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// calmNames maps serene's brokers to project_calm's. Fyers isn't one of its.
var calmNames = map[string]string{"zerodha": "kite", "upstox": "upstox", "breeze": "breeze"}

// CalmBroker is project_calm's name for a broker, and whether it uses it.
func CalmBroker(broker string) (string, bool) {
	n, ok := calmNames[broker]
	return n, ok
}

// ExportCalm writes a login into project_calm's trading.broker_sessions,
// where calm's trading and downloads read their tokens: logins are made in
// serene now. calm stores them in the clear, as it always has. A broker calm
// doesn't use is skipped; it reports whether the row was written.
func ExportCalm(ctx context.Context, calmURL string, t Token) (bool, error) {
	name, ok := CalmBroker(t.Broker)
	if !ok {
		return false, nil
	}
	if calmURL == "" {
		return false, errors.New("CALM_DATABASE_URL is not set")
	}
	cfg, err := pgx.ParseConfig(calmURL)
	if err != nil {
		return false, errors.New("CALM_DATABASE_URL is not a valid Postgres URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return false, fmt.Errorf("connect to project_calm: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	_, err = conn.Exec(ctx, `INSERT INTO trading.broker_sessions (broker, client_id, access_token, login_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (broker) DO UPDATE SET client_id = excluded.client_id, access_token = excluded.access_token,
			login_at = excluded.login_at`, name, t.UserID, t.AccessToken.Reveal(), t.IssuedAt)
	if err != nil {
		return false, fmt.Errorf("write project_calm's %s login: %w", name, err)
	}
	return true, nil
}
