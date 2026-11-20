package auth

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"serene/internal/market"
)

func TestBreezeLogin(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = r.Method + " " + r.URL.Path + " " + string(b)
		if strings.Contains(string(b), `"SessionToken":"bad"`) {
			fmt.Fprint(w, `{"Success":null,"Status":500,"Error":"Invalid session."}`)
			return
		}
		tok := base64.StdEncoding.EncodeToString([]byte("ICICI42:xyz"))
		fmt.Fprintf(w, `{"Success":{"session_token":%q},"Status":200,"Error":null}`, tok)
	}))
	defer srv.Close()
	b := &Breeze{APIKey: "k+y", BaseURL: srv.URL, Now: clock}
	if u := b.LoginURL("st"); u != srv.URL+"/apiuser/login?api_key=k%2By" {
		t.Errorf("login url %s", u)
	}
	tok, err := b.Exchange(context.Background(), url.Values{"apisession": {" 123456 "}})
	if err != nil {
		t.Fatal(err)
	}
	if body != `GET /breezeapi/api/v1/customerdetails {"AppKey":"k+y","SessionToken":"123456"}` {
		t.Errorf("sent %s", body)
	}
	if tok.UserID != "ICICI42" || tok.AccessToken.Reveal() != "123456" || !tok.ExpiresAt.Equal(market.DateOf(clock()).AddDays(1).At(6, 0)) {
		t.Errorf("%+v", tok)
	}
	if _, err := b.Exchange(context.Background(), url.Values{"apisession": {"bad"}}); err == nil || !strings.Contains(err.Error(), "the apisession is wrong") {
		t.Errorf("%v", err)
	}
}

func TestZerodhaLoginRefusedBefore0730(t *testing.T) {
	s := newServer(newFake(t), newMem())
	s.Now = func() time.Time { return market.DateOf(clock()).At(7, 10) }
	if _, err := s.Start("zerodha"); err == nil || !strings.Contains(err.Error(), "after 07:30") {
		t.Errorf("%v", err)
	}
	if _, err := s.Start("upstox"); err != nil {
		t.Error(err)
	}
	s.Now = func() time.Time { return market.DateOf(clock()).At(7, 30) }
	if _, err := s.Start("zerodha"); err != nil {
		t.Error(err)
	}
}
