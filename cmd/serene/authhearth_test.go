package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"serene/internal/auth"
)

type oneStore struct{ t auth.Token }

func (s *oneStore) Put(_ context.Context, t auth.Token) error { s.t = t; return nil }
func (s *oneStore) Get(context.Context, string) (auth.Token, error) {
	return s.t, nil
}
func (s *oneStore) Delete(context.Context, string) error { return nil }

func TestPushedLoginOpensOnlyWithHearthsKey(t *testing.T) {
	hearth := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("h", 32)))
	other := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("o", 32)))
	tok := auth.Token{Broker: "zerodha", UserID: "UF1", AccessToken: "THE-TOKEN",
		IssuedAt: time.Now().Add(-time.Hour).Truncate(time.Second), ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second)}
	args, err := putArgs(tok, hearth)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "THE-TOKEN") {
		t.Fatal("the token is in the command in the clear")
	}
	t.Setenv("CALM_DATABASE_URL", "")
	t.Setenv("SERENE_TOKEN_KEY", other)
	if err := authPut(context.Background(), &oneStore{}, args); err == nil {
		t.Fatal("opened with another key")
	}
	t.Setenv("SERENE_TOKEN_KEY", hearth)
	s := &oneStore{}
	if err := authPut(context.Background(), s, args); err != nil {
		t.Fatal(err)
	}
	if s.t.AccessToken.Reveal() != "THE-TOKEN" || s.t.UserID != "UF1" || !s.t.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Errorf("%+v", s.t)
	}
	// Sealed for zerodha, it won't open as another broker's
	args[0] = "upstox"
	if err := authPut(context.Background(), &oneStore{}, args); err == nil {
		t.Error("opened as upstox")
	}
}

func TestEnvValue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	os.WriteFile(p, []byte("# c\nA=1\nSERENE_TOKEN_KEY=\"abc=\"\n"), 0o600)
	if v, err := envValue(p, "SERENE_TOKEN_KEY"); err != nil || v != "abc=" {
		t.Errorf("%q %v", v, err)
	}
	if _, err := envValue(p, "B"); err == nil {
		t.Error("missing name found")
	}
}
