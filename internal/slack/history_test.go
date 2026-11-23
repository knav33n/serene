package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChannelHistoryDelete(t *testing.T) {
	var deleted []string
	limited := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/conversations.list":
			if q.Get("cursor") == "" {
				io.WriteString(w, `{"ok":true,"channels":[{"id":"C1","name":"other"}],"response_metadata":{"next_cursor":"p2"}}`)
			} else {
				io.WriteString(w, `{"ok":true,"channels":[{"id":"C2","name":"trades"}]}`)
			}
		case "/conversations.history":
			io.WriteString(w, `{"ok":true,"messages":[{"ts":"2.0","bot_id":"B1","reply_count":1},{"ts":"1.0","user":"U9"}]}`)
		case "/conversations.replies":
			io.WriteString(w, `{"ok":true,"messages":[{"ts":"2.0","bot_id":"B1"},{"ts":"2.5","bot_id":"B1","thread_ts":"2.0"}]}`)
		case "/chat.delete":
			if !limited {
				limited = true
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			deleted = append(deleted, in["channel"]+"/"+in["ts"])
			io.WriteString(w, `{"ok":true}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New("tok")
	c.BaseURL = srv.URL
	ctx := context.Background()
	for _, in := range []string{"#trades", "trades"} {
		if id, err := c.ChannelID(ctx, in); err != nil || id != "C2" {
			t.Fatalf("%s: %q %v", in, id, err)
		}
	}
	if id, _ := c.ChannelID(ctx, "C0CGXMPGRL7"); id != "C0CGXMPGRL7" {
		t.Errorf("an ID should pass through, got %q", id)
	}
	if _, err := c.ChannelID(ctx, "missing"); err == nil {
		t.Error("want an error for an unknown name")
	}
	h, err := c.History(ctx, "C2")
	if err != nil || len(h) != 2 {
		t.Fatalf("history %v %v", h, err)
	}
	rs, err := c.Replies(ctx, "C2", "2.0")
	if err != nil || len(rs) != 1 || rs[0].TS != "2.5" {
		t.Fatalf("replies should drop the parent: %v %v", rs, err)
	}
	if err := c.Delete(ctx, "C2", "2.5"); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "C2/2.5" {
		t.Errorf("deleted %v (want one, after the 429)", deleted)
	}
}
