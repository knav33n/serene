package manual

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"serene/internal/market"
)

func signedClick(t *testing.T, secret, user, value string, at time.Time) *http.Request {
	t.Helper()
	p, _ := json.Marshal(map[string]any{
		"type": "block_actions", "user": map[string]string{"id": user, "username": "n"},
		"channel":   map[string]string{"id": "C1"},
		"container": map[string]string{"message_ts": "111.222"},
		"actions":   []map[string]string{{"action_id": "fut_0", "value": value}},
	})
	body := url.Values{"payload": {string(p)}}.Encode()
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	r := httptest.NewRequest(http.MethodPost, "/slack/actions", strings.NewReader(body))
	r.Host = "hearth.example"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func TestSlackActions(t *testing.T) {
	now := time.Date(2026, 11, 21, 20, 0, 0, 0, market.IST)
	var mu sync.Mutex
	var placed []FutOrder
	replies := make(chan string, 10)
	a := &SlackActions{Secret: "s3cret", Users: map[string]bool{"U1": true}, Now: func() time.Time { return now },
		Place: func(_ context.Context, o FutOrder) (string, error) {
			mu.Lock()
			placed = append(placed, o)
			mu.Unlock()
			return "filled", nil
		},
		Reply: func(_ context.Context, channel, ts, text string) {
			if channel != "C1" || ts != "111.222" {
				t.Errorf("reply to %s %s", channel, ts)
			}
			replies <- text
		}}
	srv := &Server{Addr: "127.0.0.1:1", Public: "hearth.example", Slack: a}
	h := srv.Handler()
	val := FutOrder{Strategy: "goldm_hma_1h", Side: market.Sell, Symbol: "GOLDM26DECFUT"}.Value()

	do := func(r *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	// A bad signature or an old request is refused, and nothing is placed.
	bad := signedClick(t, "wrong", "U1", val, now)
	if c := do(bad); c != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", c)
	}
	if c := do(signedClick(t, "s3cret", "U1", val, now.Add(-10*time.Minute))); c != http.StatusUnauthorized {
		t.Fatalf("replay: %d", c)
	}
	// Someone not allowed is told so.
	if c := do(signedClick(t, "s3cret", "U9", val, now)); c != http.StatusOK {
		t.Fatalf("stranger: %d", c)
	}
	if r := <-replies; !strings.Contains(r, "isn't allowed") {
		t.Fatalf("stranger reply %q", r)
	}
	// The allowed user places it, once.
	if c := do(signedClick(t, "s3cret", "U1", val, now)); c != http.StatusOK {
		t.Fatalf("click: %d", c)
	}
	if r := <-replies; !strings.Contains(r, "filled") {
		t.Fatalf("reply %q", r)
	}
	do(signedClick(t, "s3cret", "U1", val, now))
	if r := <-replies; !strings.Contains(r, "already pressed") {
		t.Fatalf("second press %q", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(placed) != 1 || placed[0].Side != market.Sell || placed[0].Symbol != "GOLDM26DECFUT" || placed[0].Strategy != "goldm_hma_1h" {
		t.Fatalf("placed %+v", placed)
	}
	// The page's API still needs its checks.
	r := httptest.NewRequest(http.MethodPost, "/api/manual", strings.NewReader("{}"))
	r.Host = "hearth.example"
	if c := do(r); c != http.StatusForbidden {
		t.Fatalf("api without origin: %d", c)
	}
}

func TestFutButtons(t *testing.T) {
	b := FutButtons("silverm_hma_1h", []string{"SILVERM26FEBFUT", "SILVERM26APRFUT"}, []string{"2027-02-26", "2027-04-30"})
	els := b["elements"].([]map[string]any)
	if len(els) != 4 || els[1]["style"] != "primary" || els[2]["style"] != "danger" {
		t.Fatalf("%+v", els)
	}
	want := []FutOrder{
		{"silverm_hma_1h", market.Buy, "SILVERM26FEBFUT"}, {"silverm_hma_1h", market.Buy, "SILVERM26APRFUT"},
		{"silverm_hma_1h", market.Sell, "SILVERM26FEBFUT"}, {"silverm_hma_1h", market.Sell, "SILVERM26APRFUT"},
	}
	ids := map[string]bool{}
	for i, el := range els {
		o, err := ParseFutOrder(el["value"].(string))
		if err != nil || o != want[i] {
			t.Fatalf("button %d: %+v %v", i, o, err)
		}
		ids[el["action_id"].(string)] = true
	}
	if len(ids) != 4 {
		t.Fatalf("action ids not unique: %v", ids)
	}
}
