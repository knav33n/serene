package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDMAndUpload(t *testing.T) {
	var calls []string
	var uploaded string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.URL.Path != "/upload" && r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s: no token", r.URL.Path)
		}
		if r.URL.Path == "/upload" && r.Header.Get("Authorization") != "" {
			t.Error("the bearer token must not go to the upload URL")
		}
		switch r.URL.Path {
		case "/conversations.open":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["users"] != "U1" {
				t.Errorf("%v", in)
			}
			io.WriteString(w, `{"ok":true,"channel":{"id":"D1"}}`)
		case "/chat.postMessage":
			io.WriteString(w, `{"ok":true}`)
		case "/files.getUploadURLExternal":
			if r.URL.Query().Get("filename") != "r.html" || r.URL.Query().Get("length") != "5" || r.URL.Query().Get("alt_txt") != "chart" {
				t.Errorf("%v", r.URL.Query())
			}
			io.WriteString(w, `{"ok":true,"upload_url":"`+srv.URL+`/upload","file_id":"F1"}`)
		case "/upload":
			b, _ := io.ReadAll(r.Body)
			uploaded = string(b)
		case "/files.completeUploadExternal":
			b, _ := io.ReadAll(r.Body)
			for _, want := range []string{`"channel_id":"D1"`, `"id":"F1"`, `"initial_comment":"note"`} {
				if !strings.Contains(string(b), want) {
					t.Errorf("missing %s in %s", want, b)
				}
			}
			io.WriteString(w, `{"ok":true}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New("tok")
	c.BaseURL = srv.URL
	ctx := context.Background()
	ch, err := c.OpenDM(ctx, "U1")
	if err != nil || ch != "D1" {
		t.Fatal(ch, err)
	}
	if err := c.Post(ctx, ch, "hi"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "r.html")
	os.WriteFile(p, []byte("hello"), 0o644)
	if err := c.Upload(ctx, ch, "note", []File{{Path: p, Title: "Report", Alt: "chart"}}); err != nil {
		t.Fatal(err)
	}
	if uploaded != "hello" || len(calls) != 5 {
		t.Errorf("%q %v", uploaded, calls)
	}
}

func TestErrorsNeverEchoTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":false,"error":"missing_scope","needed":"files:write"}`)
	}))
	defer srv.Close()
	c := New("xoxb-secret")
	c.BaseURL = srv.URL
	err := c.Post(context.Background(), "D1", "hi")
	if err == nil || !strings.Contains(err.Error(), "add files:write") || strings.Contains(err.Error(), "xoxb") {
		t.Errorf("%v", err)
	}
}

func TestResolve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"channel":{"id":"D9"}}`)
	}))
	defer srv.Close()
	c := New("tok")
	c.BaseURL = srv.URL
	ctx := context.Background()
	if got, err := c.Resolve(ctx, "U123"); err != nil || got != "D9" {
		t.Errorf("member ID: %q %v", got, err)
	}
	if got, err := c.Resolve(ctx, "C123"); err != nil || got != "C123" {
		t.Errorf("channel ID: %q %v", got, err)
	}
	if _, err := c.Resolve(ctx, "#markets"); err == nil {
		t.Error("a channel name must be rejected")
	}
}
