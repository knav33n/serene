package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// calmBrokers maps project_calm's broker names to serene's, with the IST hour
// and minute each broker's tokens stop working.
var calmBrokers = map[string]struct {
	name         string
	hour, minute int
}{
	"kite":   {"zerodha", 6, 0},
	"upstox": {"upstox", 3, 30},
}

// CalmSession is one row of project_calm's trading.broker_sessions.
type CalmSession struct {
	Broker, ClientID string
	AccessToken      Secret
	LoginAt          time.Time
}

// FromCalm turns a project_calm session into a serene token. It reports false
// for a broker serene doesn't use.
func FromCalm(s CalmSession) (Token, bool) {
	b, ok := calmBrokers[s.Broker]
	if !ok {
		return Token{}, false
	}
	return Token{Broker: b.name, UserID: s.ClientID, AccessToken: s.AccessToken,
		IssuedAt: s.LoginAt, ExpiresAt: nextCutoff(s.LoginAt, b.hour, b.minute)}, true
}

// ImportCalm copies the brokers' sessions from project_calm, where the user
// logs in each morning, into s. A session replaces the stored token only when
// it is newer. It returns the brokers it updated.
func ImportCalm(ctx context.Context, calmURL string, s Store) ([]string, error) {
	if calmURL == "" {
		return nil, errors.New("CALM_DATABASE_URL is not set")
	}
	cfg, err := pgx.ParseConfig(calmURL)
	if err != nil {
		return nil, errors.New("CALM_DATABASE_URL is not a valid Postgres URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to project_calm: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	rows, err := conn.Query(ctx, `SELECT broker, client_id, access_token, login_at FROM trading.broker_sessions`)
	if err != nil {
		return nil, fmt.Errorf("read project_calm sessions: %w", err)
	}
	var sessions []CalmSession
	for rows.Next() {
		var c CalmSession
		var tok string
		if err := rows.Scan(&c.Broker, &c.ClientID, &tok, &c.LoginAt); err != nil {
			return nil, err
		}
		c.AccessToken = Secret(tok)
		sessions = append(sessions, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var updated []string
	for _, c := range sessions {
		t, ok := FromCalm(c)
		if !ok {
			continue
		}
		old, err := s.Get(ctx, t.Broker)
		if err != nil && !errors.Is(err, ErrNoToken) {
			return updated, err
		}
		// Only a newer login replaces the stored one: an older project_calm
		// session would otherwise overwrite a fresh serene auth login.
		if err == nil && !t.IssuedAt.After(old.IssuedAt) {
			continue
		}
		if err := s.Put(ctx, t); err != nil {
			return updated, err
		}
		updated = append(updated, t.Broker)
	}
	return updated, nil
}
