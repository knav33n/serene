package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"serene/internal/market"
)

func TestFyersLogin(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/validate-authcode" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"s":"ok","code":200,"message":"","access_token":"AT","refresh_token":"RT"}`)
	}))
	defer srv.Close()
	issued := market.Date{Year: 2026, Month: time.November, Day: 16}.At(9, 0)
	f := &Fyers{AppID: "APP-100", Secret: "sec", RedirectURI: "http://127.0.0.1:8080/callback/fyers",
		BaseURL: srv.URL, Now: func() time.Time { return issued }}

	login, _ := url.Parse(f.LoginURL("st"))
	if q := login.Query(); !strings.HasSuffix(login.Path, "/generate-authcode") || q.Get("client_id") != "APP-100" ||
		q.Get("state") != "st" || q.Get("response_type") != "code" || q.Get("redirect_uri") != f.RedirectURI {
		t.Fatalf("login URL %s", login)
	}
	tok, err := f.Exchange(context.Background(), url.Values{"auth_code": {"AC"}, "state": {"st"}})
	if err != nil {
		t.Fatal(err)
	}
	// sha256("APP-100:sec")
	if got["code"] != "AC" || got["grant_type"] != "authorization_code" || got["appIdHash"] != appIDHash("APP-100", "sec") || len(got["appIdHash"]) != 64 {
		t.Fatalf("request %v", got)
	}
	if tok.AccessToken.Reveal() != "AT" || tok.Broker != "fyers" ||
		!tok.ExpiresAt.Equal(market.Date{Year: 2026, Month: time.November, Day: 17}.At(6, 30)) {
		t.Fatalf("token %+v expires %s", tok, tok.ExpiresAt)
	}
	if _, err := f.Exchange(context.Background(), url.Values{}); err == nil {
		t.Fatal("exchanged without a code")
	}
}

func TestFyersRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"s":"error","code":-413,"message":"invalid auth code"}`)
	}))
	defer srv.Close()
	f := &Fyers{AppID: "APP-100", Secret: "sec", BaseURL: srv.URL}
	_, err := f.Exchange(context.Background(), url.Values{"auth_code": {"AC"}})
	if err == nil || !strings.Contains(err.Error(), "invalid auth code") {
		t.Fatalf("err %v", err)
	}
}
