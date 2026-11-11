package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoToken means no login is stored for a broker.
var ErrNoToken = errors.New("no token stored")

// Store keeps each broker's current token.
type Store interface {
	Put(ctx context.Context, t Token) error
	Get(ctx context.Context, broker string) (Token, error)
	Delete(ctx context.Context, broker string) error
}

// Load returns a usable token, for the broker clients to call. Anything else
// comes back as an error saying what to do.
func Load(ctx context.Context, s Store, broker string) (Token, error) {
	t, err := s.Get(ctx, broker)
	if errors.Is(err, ErrNoToken) {
		return Token{}, fmt.Errorf("not logged in to %s: run serene auth", broker)
	}
	if err != nil {
		return Token{}, err
	}
	if !t.Valid(time.Now()) {
		return Token{}, fmt.Errorf("%s token expired at %s: run serene auth", broker, t.ExpiresAt.Format(time.RFC3339))
	}
	return t, nil
}

// PGStore keeps tokens in Postgres, encrypted before they leave this machine.
type PGStore struct {
	pool  *pgxpool.Pool
	seal  *Sealer
	table string
}

// OpenPostgres connects and creates the schema and table if missing. The URL
// holds the password, so parse errors are never passed on.
func OpenPostgres(ctx context.Context, databaseURL string, seal *Sealer, schema string) (*PGStore, error) {
	if databaseURL == "" {
		return nil, errors.New("SERENE_DATABASE_URL is not set")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("SERENE_DATABASE_URL is not a valid Postgres URL")
	}
	cfg.MaxConns = 4
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	s := &PGStore{pool: pool, seal: seal, table: pgx.Identifier{schema, "broker_tokens"}.Sanitize()}
	ddl := `CREATE SCHEMA IF NOT EXISTS ` + pgx.Identifier{schema}.Sanitize() + `;
		CREATE TABLE IF NOT EXISTS ` + s.table + ` (
			broker           text PRIMARY KEY,
			user_id          text NOT NULL,
			token_ciphertext bytea NOT NULL,
			issued_at        timestamptz NOT NULL,
			expires_at       timestamptz NOT NULL,
			updated_at       timestamptz NOT NULL DEFAULT now()
		)`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		pool.Close()
		return nil, fmt.Errorf("prepare token table: %w", err)
	}
	return s, nil
}

// Close releases the connections.
func (s *PGStore) Close() { s.pool.Close() }

// TLS reports whether the database connection is encrypted.
func (s *PGStore) TLS(ctx context.Context) (bool, error) {
	var on bool
	err := s.pool.QueryRow(ctx, `SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()`).Scan(&on)
	return on, err
}

// Put replaces the broker's token.
func (s *PGStore) Put(ctx context.Context, t Token) error {
	sealed, err := s.seal.Seal(t.Broker, t.AccessToken)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO `+s.table+` (broker, user_id, token_ciphertext, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (broker) DO UPDATE SET user_id = EXCLUDED.user_id,
			token_ciphertext = EXCLUDED.token_ciphertext, issued_at = EXCLUDED.issued_at,
			expires_at = EXCLUDED.expires_at, updated_at = now()`,
		t.Broker, t.UserID, sealed, t.IssuedAt, t.ExpiresAt)
	if err != nil {
		return fmt.Errorf("store token: %w", err)
	}
	return nil
}

// Get returns the broker's token.
func (s *PGStore) Get(ctx context.Context, broker string) (Token, error) {
	t := Token{Broker: broker}
	var sealed []byte
	err := s.pool.QueryRow(ctx, `SELECT user_id, token_ciphertext, issued_at, expires_at FROM `+s.table+` WHERE broker = $1`, broker).
		Scan(&t.UserID, &sealed, &t.IssuedAt, &t.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNoToken
	}
	if err != nil {
		return Token{}, fmt.Errorf("read token: %w", err)
	}
	t.AccessToken, err = s.seal.Open(broker, sealed)
	if err != nil {
		return Token{}, err
	}
	return t, nil
}

// Delete removes the broker's token.
func (s *PGStore) Delete(ctx context.Context, broker string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM `+s.table+` WHERE broker = $1`, broker); err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return nil
}
