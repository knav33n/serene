package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"calm/internal/market"
)

const (
	realToken  = "REAL-ACCESS-TOKEN-1234"
	realSecret = "REAL-API-SECRET-5678"
	addr       = "127.0.0.1:8080"
	origin     = "http://127.0.0.1:8080"
	// printf 'kitekeyreqtokensecretvalue' | sha256sum
	kiteChecksum = "d09c3c60e02f9930833c5eea099e36f8aa5cabae10a988ee10bed6ac9f83e555"
)

var testKey = base64.StdEncoding.EncodeToString(make([]byte, 32))

type memStore struct {
	mu   sync.Mutex
	rows map[string]Token
	fail error
}

func newMem() *memStore { return &memStore{rows: map[string]Token{}} }

func (m *memStore) Put(_ context.Context, t Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.rows[t.Broker] = t
	return nil
}

func (m *memStore) Get(_ context.Context, b string) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.rows[b]
	if !ok {
		return Token{}, ErrNoToken
	}
	return t, nil
}

func (m *memStore) Delete(_ context.Context, b string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, b)
	return nil
}

// fakeBroker answers both brokers' APIs and records what it was sent.
type fakeBroker struct {
	*httptest.Server
	mu         sync.Mutex
	exchange   int // status for token exchange
	revoke     int // status for revoke
	revoked    int
	form       url.Values
	kiteHeader string
	bearer     string
	deleteArgs url.Values
}

func newFake(t *testing.T) *fakeBroker {
	f := &fakeBroker{exchange: 200, revoke: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/login/authorization/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.form = r.PostForm
		if f.exchange != 200 {
			w.WriteHeader(f.exchange)
			fmt.Fprint(w, `{"status":"error","errors":[{"errorCode":"UDAPI100057","message":"Invalid code"}]}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"user_id":"UPX1"}`, realToken)
	})
	mux.HandleFunc("DELETE /v2/logout", func(w http.ResponseWriter, r *http.Request) {
		f.bearer = r.Header.Get("Authorization")
		f.revoked++
		w.WriteHeader(f.revoke)
		fmt.Fprint(w, `{"status":"success","data":true}`)
	})
	mux.HandleFunc("POST /session/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.form = r.PostForm
		f.kiteHeader = r.Header.Get("X-Kite-Version")
		if f.exchange != 200 {
			w.WriteHeader(f.exchange)
			fmt.Fprint(w, `{"status":"error","message":"Token is invalid or has expired.","error_type":"TokenException"}`)
			return
		}
		fmt.Fprintf(w, `{"status":"success","data":{"access_token":%q,"user_id":"ZD1"}}`, realToken)
	})
	mux.HandleFunc("DELETE /session/token", func(w http.ResponseWriter, r *http.Request) {
		f.deleteArgs = r.URL.Query()
		f.revoked++
		w.WriteHeader(f.revoke)
		fmt.Fprint(w, `{"status":"success","data":true}`)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

var clock = func() time.Time { d, _ := market.ParseDate("2026-11-06"); return d.At(8, 0) }

func newServer(f *fakeBroker, store Store) *Server {
	return &Server{Addr: addr, Store: store, Now: clock, Grace: time.Hour,
		UI: fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}, "assets/a.js": {Data: []byte("js")}},
		Providers: map[string]Provider{
			"upstox":  &Upstox{APIKey: "upkey", APISecret: realSecret, RedirectURI: origin + "/callback/upstox", BaseURL: f.URL, Now: clock},
			"zerodha": &Zerodha{APIKey: "kitekey", APISecret: "secretvalue", LoginBase: f.URL, APIBase: f.URL, Now: clock},
		}}
}

func call(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+addr+path, nil)
	r.Host = addr
	if method != http.MethodGet {
		r.Header.Set("Origin", origin)
	}
	for k, v := range headers {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// startLogin returns the state value from the broker URL the page would use.
func startLogin(t *testing.T, h http.Handler, broker string) string {
	t.Helper()
	w := call(h, "POST", "/api/brokers/"+broker+"/login", nil)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var r struct{ URL string }
	json.Unmarshal(w.Body.Bytes(), &r)
	u, err := url.Parse(r.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rp := u.Query().Get("redirect_params"); rp != "" {
		v, _ := url.ParseQuery(rp)
		return v.Get("state")
	}
	return u.Query().Get("state")
}

func brokerState(t *testing.T, h http.Handler, name string) Status {
	t.Helper()
	var out []Status
	json.Unmarshal(call(h, "GET", "/api/brokers", nil).Body.Bytes(), &out)
	for _, s := range out {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status for %s", name)
	return Status{}
}

func TestUpstoxLogin(t *testing.T) {
	f, store := newFake(t), newMem()
	h := newServer(f, store).Handler()
	state := startLogin(t, h, "upstox")
	w := call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "result=ok") {
		t.Fatalf("callback %d %s", w.Code, w.Header().Get("Location"))
	}
	got := store.rows["upstox"]
	if got.AccessToken.Reveal() != realToken || got.UserID != "UPX1" {
		t.Fatalf("stored %+v", got)
	}
	if !got.ExpiresAt.Equal(market.DateOf(clock()).AddDays(1).At(3, 30)) {
		t.Errorf("expiry %s", got.ExpiresAt)
	}
	want := map[string]string{"code": "abc", "client_id": "upkey", "client_secret": realSecret,
		"redirect_uri": origin + "/callback/upstox", "grant_type": "authorization_code"}
	for k, v := range want {
		if f.form.Get(k) != v {
			t.Errorf("%s = %q", k, f.form.Get(k))
		}
	}
	if s := brokerState(t, h, "upstox"); s.State != "active" || s.UserID != "UPX1" {
		t.Errorf("status %+v", s)
	}
}

func TestZerodhaLoginUsesHandComputedChecksum(t *testing.T) {
	f, store := newFake(t), newMem()
	h := newServer(f, store).Handler()
	state := startLogin(t, h, "zerodha")
	if len(state) != 64 {
		t.Fatalf("state %q did not round trip through redirect_params", state)
	}
	call(h, "GET", "/callback/zerodha?action=login&status=success&request_token=reqtoken&state="+state, nil)
	if f.form.Get("checksum") != kiteChecksum || f.kiteHeader != "3" {
		t.Errorf("checksum %q version %q", f.form.Get("checksum"), f.kiteHeader)
	}
	got := store.rows["zerodha"]
	if got.AccessToken.Reveal() != realToken || !got.ExpiresAt.Equal(market.DateOf(clock()).AddDays(1).At(6, 0)) {
		t.Fatalf("stored %+v", got)
	}
}

func TestBadCallbacksStoreNothing(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fakeBroker)
		path  func(state string) string
	}{
		{"wrong state", nil, func(string) string { return "/callback/upstox?code=abc&state=forged" }},
		{"no state", nil, func(string) string { return "/callback/upstox?code=abc" }},
		{"state from other broker", nil, func(s string) string { return "/callback/zerodha?request_token=x&state=" + s }},
		{"denied login", nil, func(s string) string { return "/callback/upstox?error=access_denied&state=" + s }},
		{"failed exchange", func(f *fakeBroker) { f.exchange = 400 }, func(s string) string { return "/callback/upstox?code=abc&state=" + s }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, store := newFake(t), newMem()
			if c.setup != nil {
				c.setup(f)
			}
			h := newServer(f, store).Handler()
			state := startLogin(t, h, "upstox")
			w := call(h, "GET", c.path(state), nil)
			if !strings.Contains(w.Header().Get("Location"), "result=failed") {
				t.Errorf("location %s", w.Header().Get("Location"))
			}
			if len(store.rows) != 0 {
				t.Errorf("stored %+v", store.rows)
			}
		})
	}
}

func TestStateIsSingleUseAndExpires(t *testing.T) {
	f, store := newFake(t), newMem()
	s := newServer(f, store)
	h := s.Handler()
	state := startLogin(t, h, "upstox")
	call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	delete(store.rows, "upstox")
	call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	if len(store.rows) != 0 {
		t.Error("state worked twice")
	}

	state = startLogin(t, h, "upstox")
	s.Now = func() time.Time { return clock().Add(pendingTTL + time.Second) }
	call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	if len(store.rows) != 0 {
		t.Error("expired state worked")
	}
}

func paste(h http.Handler, broker, value string, headers map[string]string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"value": value})
	r := httptest.NewRequest("POST", "http://"+addr+"/api/brokers/"+broker+"/token", strings.NewReader(string(b)))
	r.Host = addr
	r.Header.Set("Origin", origin)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPastedToken(t *testing.T) {
	t.Run("bare zerodha token", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		startLogin(t, h, "zerodha")
		if w := paste(h, "zerodha", "  reqtoken\n", nil); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if f.form.Get("checksum") != kiteChecksum || store.rows["zerodha"].UserID != "ZD1" {
			t.Errorf("checksum %q rows %v", f.form.Get("checksum"), store.rows)
		}
	})
	t.Run("bare upstox code", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		startLogin(t, h, "upstox")
		paste(h, "upstox", "abc", nil)
		if f.form.Get("code") != "abc" || store.rows["upstox"].UserID != "UPX1" {
			t.Errorf("code %q rows %v", f.form.Get("code"), store.rows)
		}
	})
	t.Run("whole redirected URL", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		state := startLogin(t, h, "zerodha")
		w := paste(h, "zerodha", "https://example.com/cb?action=login&status=success&request_token=reqtoken&state="+state, nil)
		if w.Code != 200 || f.form.Get("checksum") != kiteChecksum {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("redirected URL without state", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		startLogin(t, h, "zerodha")
		if w := paste(h, "zerodha", "https://example.com/cb?request_token=reqtoken", nil); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("URL from another login is refused", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		startLogin(t, h, "zerodha")
		if w := paste(h, "zerodha", "https://example.com/cb?request_token=x&state=forged", nil); w.Code != http.StatusConflict {
			t.Fatalf("%d", w.Code)
		}
		if len(store.rows) != 0 {
			t.Error("stored a token")
		}
	})
	t.Run("URL without the token", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		startLogin(t, h, "zerodha")
		if w := paste(h, "zerodha", "https://example.com/cb?status=cancelled", nil); w.Code != http.StatusBadRequest {
			t.Fatalf("%d", w.Code)
		}
	})
	t.Run("needs a login in progress", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		if w := paste(h, "zerodha", "reqtoken", nil); w.Code != http.StatusConflict {
			t.Fatalf("%d", w.Code)
		}
		startLogin(t, h, "upstox")
		if w := paste(h, "zerodha", "reqtoken", nil); w.Code != http.StatusConflict {
			t.Fatalf("other broker's login accepted: %d", w.Code)
		}
	})
	t.Run("login expires", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		h := s.Handler()
		startLogin(t, h, "zerodha")
		s.Now = func() time.Time { return clock().Add(pendingTTL + time.Second) }
		if w := paste(h, "zerodha", "reqtoken", nil); w.Code != http.StatusConflict {
			t.Fatalf("%d", w.Code)
		}
	})
	t.Run("refused token can be retried, then login closes", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		startLogin(t, h, "zerodha")
		f.exchange = 403
		w := paste(h, "zerodha", "typo", nil)
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "typo") || len(store.rows) != 0 {
			t.Fatalf("%d %s %v", w.Code, w.Body, store.rows)
		}
		f.exchange = 200
		if w := paste(h, "zerodha", "reqtoken", nil); w.Code != 200 {
			t.Fatalf("retry: %d", w.Code)
		}
		if w := paste(h, "zerodha", "reqtoken", nil); w.Code != http.StatusConflict {
			t.Fatalf("login stayed open: %d", w.Code)
		}
	})
	t.Run("foreign origin is refused", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		startLogin(t, h, "zerodha")
		if w := paste(h, "zerodha", "reqtoken", map[string]string{"Origin": "https://evil.example"}); w.Code != http.StatusForbidden {
			t.Fatalf("%d", w.Code)
		}
	})
	t.Run("status never includes the token", func(t *testing.T) {
		f, store := newFake(t), newMem()
		h := newServer(f, store).Handler()
		startLogin(t, h, "zerodha")
		w := paste(h, "zerodha", "reqtoken", nil)
		if strings.Contains(w.Body.String(), realToken) || !strings.Contains(w.Body.String(), `"token_param":"request_token"`) {
			t.Fatalf("%s", w.Body)
		}
	})
}

func TestGuards(t *testing.T) {
	f, store := newFake(t), newMem()
	store.rows["upstox"] = Token{Broker: "upstox", AccessToken: realToken, ExpiresAt: clock().Add(time.Hour)}
	h := newServer(f, store).Handler()
	cases := []struct {
		name, method, path string
		headers            map[string]string
	}{
		{"rebinding host on status", "GET", "/api/brokers", map[string]string{"Host": "evil.example:8080"}},
		{"rebinding host on callback", "GET", "/callback/upstox", map[string]string{"Host": "evil.example"}},
		{"foreign origin login", "POST", "/api/brokers/upstox/login", map[string]string{"Origin": "https://evil.example"}},
		{"foreign origin logout", "POST", "/api/brokers/upstox/logout", map[string]string{"Origin": "https://evil.example"}},
		{"missing origin logout", "POST", "/api/brokers/upstox/logout", map[string]string{"Origin": ""}},
	}
	for _, c := range cases {
		if w := call(h, c.method, c.path, c.headers); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", c.name, w.Code)
		}
	}
	if f.revoked != 0 || len(store.rows) != 1 {
		t.Error("a blocked request had an effect")
	}
	w := call(h, "GET", "/", nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "app") {
		t.Errorf("page: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := call(h, "HEAD", "/", map[string]string{"Origin": ""}); w.Code != 200 {
		t.Errorf("HEAD: %d", w.Code)
	}
	if w := call(h, "GET", "/assets/a.js", nil); w.Body.String() != "js" {
		t.Errorf("asset: %q", w.Body)
	}
	if w := call(h, "GET", "/../../etc/passwd", nil); w.Code == 200 {
		t.Error("path traversal served")
	}
}

func TestLogout(t *testing.T) {
	active := func() *memStore {
		s := newMem()
		s.rows["upstox"] = Token{Broker: "upstox", UserID: "U", AccessToken: realToken, ExpiresAt: clock().Add(time.Hour)}
		s.rows["zerodha"] = Token{Broker: "zerodha", UserID: "Z", AccessToken: realToken, ExpiresAt: clock().Add(time.Hour)}
		return s
	}
	t.Run("revokes then deletes", func(t *testing.T) {
		f, store := newFake(t), active()
		h := newServer(f, store).Handler()
		if w := call(h, "POST", "/api/brokers/upstox/logout", nil); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if f.bearer != "Bearer "+realToken || f.revoked != 1 {
			t.Errorf("bearer %q revoked %d", f.bearer, f.revoked)
		}
		if _, ok := store.rows["upstox"]; ok {
			t.Error("row kept")
		}
		call(h, "POST", "/api/brokers/zerodha/logout", nil)
		if f.deleteArgs.Get("access_token") != realToken || f.deleteArgs.Get("api_key") != "kitekey" {
			t.Errorf("kite args %v", f.deleteArgs)
		}
	})
	t.Run("failed revoke keeps the row", func(t *testing.T) {
		f, store := newFake(t), active()
		f.revoke = 500
		h := newServer(f, store).Handler()
		w := call(h, "POST", "/api/brokers/upstox/logout", nil)
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "may still work") {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if _, ok := store.rows["upstox"]; !ok {
			t.Error("row deleted though the token may still work")
		}
	})
	t.Run("expired token skips the broker", func(t *testing.T) {
		f, store := newFake(t), active()
		tok := store.rows["upstox"]
		tok.ExpiresAt = clock().Add(-time.Hour)
		store.rows["upstox"] = tok
		call(newServer(f, store).Handler(), "POST", "/api/brokers/upstox/logout", nil)
		if f.revoked != 0 || store.rows["upstox"].Broker != "" {
			t.Errorf("revoked %d rows %v", f.revoked, store.rows)
		}
	})
}

func TestNothingLeaksToTheBrowserOrLogs(t *testing.T) {
	f, store := newFake(t), newMem()
	s := newServer(f, store)
	h := s.Handler()
	var seen strings.Builder
	state := startLogin(t, h, "upstox")
	w := call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	seen.WriteString(w.Header().Get("Location") + w.Body.String())
	seen.WriteString(call(h, "GET", "/api/brokers", nil).Body.String())
	seen.WriteString(call(h, "POST", "/api/brokers/upstox/login", nil).Body.String())
	f.exchange = 400
	state = startLogin(t, h, "upstox")
	call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	seen.WriteString(call(h, "GET", "/api/brokers", nil).Body.String())

	tok := Token{Broker: "b", AccessToken: realToken}
	b, _ := json.Marshal(tok)
	seen.Write(b)
	fmt.Fprintf(&seen, "%v %+v %#v %s", tok, tok, tok, tok.AccessToken)
	seen.WriteString(fmt.Sprintf("%+v", s.Providers["upstox"]))

	// Kite's revoke puts the token in the URL; a transport error must not show it.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	z := &Zerodha{APIKey: "k", APIBase: dead.URL}
	err := z.Revoke(context.Background(), tok)
	if err == nil {
		t.Fatal("revoke should fail")
	}
	seen.WriteString(err.Error())

	for _, secret := range []string{realToken, realSecret} {
		if strings.Contains(seen.String(), secret) {
			t.Errorf("%s leaked in: %s", secret, seen.String())
		}
	}
}

func TestExpiry(t *testing.T) {
	d, _ := market.ParseDate("2026-11-10")
	cases := []struct {
		issued       time.Time
		hour, minute int
		want         time.Time
	}{
		{d.At(20, 0), 3, 30, d.AddDays(1).At(3, 30)},
		{d.At(2, 30), 3, 30, d.At(3, 30)},
		{d.At(3, 29), 3, 30, d.At(3, 30)},
		{d.At(3, 30), 3, 30, d.AddDays(1).At(3, 30)},
		{d.At(5, 59), 6, 0, d.At(6, 0)},
		{d.At(6, 0), 6, 0, d.AddDays(1).At(6, 0)},
		{d.At(9, 0), 6, 0, d.AddDays(1).At(6, 0)},
	}
	for _, c := range cases {
		if got := nextCutoff(c.issued, c.hour, c.minute); !got.Equal(c.want) {
			t.Errorf("issued %s: got %s want %s", c.issued, got, c.want)
		}
	}
	morning := Token{ExpiresAt: nextCutoff(d.At(8, 0), 3, 30)}
	if !morning.ValidThrough(d.At(15, 30)) {
		t.Error("a morning token should last the session")
	}
	night := Token{ExpiresAt: nextCutoff(d.At(2, 30), 3, 30)}
	if night.ValidThrough(d.At(15, 30)) || !night.Valid(d.At(3, 0)) || night.Valid(d.At(3, 30)) {
		t.Error("a token issued at 02:30 is valid briefly and can't cover the session")
	}
}

func TestSealer(t *testing.T) {
	s, err := NewSealer(testKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Seal("upstox", realToken)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), realToken) {
		t.Error("ciphertext contains the token")
	}
	got, err := s.Open("upstox", sealed)
	if err != nil || got.Reveal() != realToken {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := s.Open("zerodha", sealed); err == nil {
		t.Error("ciphertext opened under another broker")
	}
	other := make([]byte, 32)
	other[0] = 1
	s2, _ := NewSealer(base64.StdEncoding.EncodeToString(other))
	if _, err := s2.Open("upstox", sealed); err == nil {
		t.Error("wrong key opened the token")
	}
	if _, err := s.Open("upstox", sealed[:4]); err == nil {
		t.Error("truncated data accepted")
	}
	second, _ := s.Seal("upstox", realToken)
	if string(second) == string(sealed) {
		t.Error("nonce reused")
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := NewSealer(bad); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
}

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# comment\n\nCALM_T_A=one\nexport CALM_T_B=\"two words\"\nCALM_T_C='p=q'\nCALM_T_SET=file\n"), 0o600)
	t.Setenv("CALM_T_SET", "real")
	for _, k := range []string{"CALM_T_A", "CALM_T_B", "CALM_T_C"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"CALM_T_A": "one", "CALM_T_B": "two words", "CALM_T_C": "p=q", "CALM_T_SET": "real"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q", k, got)
		}
		defer os.Unsetenv(k)
	}
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("missing file: %v", err)
	}
	os.WriteFile(path, []byte("SECRETVALUEWITHNOEQUALS\n"), 0o600)
	err := LoadDotEnv(path)
	if err == nil || strings.Contains(err.Error(), "SECRETVALUE") {
		t.Errorf("error should name the line, not its contents: %v", err)
	}
}

func TestRunStopsWhenAllBrokersAreLoggedIn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	f, store := newFake(t), newMem()
	s := newServer(f, store)
	s.Addr, s.Grace, s.Idle, s.Now = a, 50*time.Millisecond, time.Hour, nil
	delete(s.Providers, "zerodha")
	s.Providers["upstox"].(*Upstox).Now = nil
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	base := "http://" + a
	var resp *http.Response
	for range 100 {
		req, _ := http.NewRequest("POST", base+"/api/brokers/upstox/login", nil)
		req.Header.Set("Origin", base)
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	var r struct{ URL string }
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	u, _ := url.Parse(r.URL)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cb, err := noRedirect.Get(base + "/callback/upstox?code=x&state=" + u.Query().Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	cb.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after the last login")
	}
	if len(store.rows) != 1 {
		t.Errorf("rows %v", store.rows)
	}
}

func TestUpstoxRedirectURI(t *testing.T) {
	t.Setenv("UPSTOX_API_KEY", "k")
	t.Setenv("UPSTOX_API_SECRET", "s")
	t.Setenv("UPSTOX_REDIRECT_URI", "")
	login := func() string {
		u, err := url.Parse(ProvidersFromEnv(origin)["upstox"].LoginURL("st"))
		if err != nil {
			t.Fatal(err)
		}
		return u.Query().Get("redirect_uri")
	}
	if got := login(); got != origin+"/callback/upstox" {
		t.Errorf("default: %q", got)
	}
	t.Setenv("UPSTOX_REDIRECT_URI", "https://example.com/cb/")
	if got := login(); got != "https://example.com/cb/" {
		t.Errorf("override must be sent unchanged: %q", got)
	}
}

func TestRunRefusesNonLoopback(t *testing.T) {
	s := &Server{Addr: "0.0.0.0:8080"}
	if err := s.Run(context.Background()); err == nil {
		t.Error("wildcard address accepted")
	}
}
