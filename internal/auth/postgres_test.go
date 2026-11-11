package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Runs only when SERENE_TEST_DATABASE_URL is set, in a temporary schema.
func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("SERENE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SERENE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	rand.Read(b)
	schema := "serene_test_" + hex.EncodeToString(b)
	seal, _ := NewSealer(testKey)
	s, err := OpenPostgres(ctx, dsn, seal, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.pool.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)

	if _, err := s.Get(ctx, "upstox"); !errors.Is(err, ErrNoToken) {
		t.Fatalf("empty store: %v", err)
	}
	issued := time.Now().Truncate(time.Microsecond)
	tok := Token{Broker: "upstox", UserID: "U1", AccessToken: realToken, IssuedAt: issued, ExpiresAt: issued.Add(time.Hour)}
	if err := s.Put(ctx, tok); err != nil {
		t.Fatal(err)
	}
	tok.UserID = "U2"
	if err := s.Put(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "upstox")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != "U2" || got.AccessToken.Reveal() != realToken || !got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Fatalf("got %+v", got)
	}

	var rows int
	var stored []byte
	err = s.pool.QueryRow(ctx, `SELECT count(*) OVER (), token_ciphertext FROM `+s.table).Scan(&rows, &stored)
	if err != nil || rows != 1 {
		t.Fatalf("rows %d err %v", rows, err)
	}
	if strings.Contains(string(stored), realToken) {
		t.Error("the database holds the token in plain text")
	}

	if _, err := s.pool.Exec(ctx, `UPDATE `+s.table+` SET broker = 'zerodha'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "zerodha"); err == nil {
		t.Error("a row moved to another broker still decrypted")
	}
	if _, err := Load(ctx, s, "upstox"); err == nil || !strings.Contains(err.Error(), "run serene auth") {
		t.Errorf("load: %v", err)
	}
	if err := s.Delete(ctx, "zerodha"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "zerodha"); !errors.Is(err, ErrNoToken) {
		t.Errorf("after delete: %v", err)
	}
}

func TestPostgresErrorsHideTheURL(t *testing.T) {
	seal, _ := NewSealer(testKey)
	_, err := OpenPostgres(context.Background(), "postgres://u:hunter2@bad host:notaport/db", seal, "serene")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("got %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = OpenPostgres(ctx, "postgres://u:hunter2@127.0.0.1:1/db", seal, "serene")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("got %v", err)
	}
	if _, err := OpenPostgres(ctx, "", seal, "serene"); err == nil {
		t.Error("empty URL accepted")
	}
}
