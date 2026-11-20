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

	"serene/internal/market"
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
	return &Server{Addr: addr, Store: store, Now: clock,
		Providers: map[string]Provider{
			"upstox":  &Upstox{APIKey: "upkey", APISecret: realSecret, RedirectURI: origin + "/callback/upstox", BaseURL: f.URL, Now: clock},
			"zerodha": &Zerodha{APIKey: "kitekey", APISecret: "secretvalue", LoginBase: f.URL, APIBase: f.URL, Now: clock},
		}}
}

func call(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+addr+path, nil)
	r.Host = addr
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

// startLogin returns the state value from the broker URL serene auth prints.
func startLogin(t *testing.T, s *Server, broker string) string {
	t.Helper()
	raw, err := s.Start(broker)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rp := u.Query().Get("redirect_params"); rp != "" {
		v, _ := url.ParseQuery(rp)
		return v.Get("state")
	}
	return u.Query().Get("state")
}

func brokerState(t *testing.T, s *Server, name string) Status {
	t.Helper()
	for _, st := range s.Statuses(context.Background()) {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("no status for %s", name)
	return Status{}
}

func TestUpstoxLogin(t *testing.T) {
	f, store := newFake(t), newMem()
	s := newServer(f, store)
	h := s.Handler()
	state := startLogin(t, s, "upstox")
	w := call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?done=upstox" || !s.Done("upstox") {
		t.Fatalf("callback %d %s", w.Code, w.Body)
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
	if st := brokerState(t, s, "upstox"); st.State != "active" || st.UserID != "UPX1" {
		t.Errorf("status %+v", st)
	}
}

func TestZerodhaLoginUsesHandComputedChecksum(t *testing.T) {
	f, store := newFake(t), newMem()
	s := newServer(f, store)
	state := startLogin(t, s, "zerodha")
	if len(state) != 64 {
		t.Fatalf("state %q did not round trip through redirect_params", state)
	}
	call(s.Handler(), "GET", "/callback/zerodha?action=login&status=success&request_token=reqtoken&state="+state, nil)
	if f.form.Get("checksum") != kiteChecksum || f.kiteHeader != "3" {
		t.Errorf("checksum %q version %q", f.form.Get("checksum"), f.kiteHeader)
	}
	got := store.rows["zerodha"]
	if got.AccessToken.Reveal() != realToken || !got.ExpiresAt.Equal(market.DateOf(clock()).AddDays(1).At(6, 0)) {
		t.Fatalf("stored %+v", got)
	}
}

func TestStartNeedsTheBrokersKeys(t *testing.T) {
	s := newServer(newFake(t), newMem())
	if _, err := s.Start("fyers"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("%v", err)
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
			s := newServer(f, store)
			state := startLogin(t, s, "upstox")
			w := call(s.Handler(), "GET", c.path(state), nil)
			name := strings.TrimPrefix(strings.Split(c.path(state), "?")[0], "/callback/")
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?failed="+name || s.LastError(name) == "" {
				t.Errorf("%d %s %q", w.Code, w.Header().Get("Location"), s.LastError("upstox"))
			}
			if len(store.rows) != 0 || s.Done("upstox") {
				t.Errorf("stored %+v", store.rows)
			}
		})
	}
}

func TestStateIsSingleUseAndExpires(t *testing.T) {
	f, store := newFake(t), newMem()
	s := newServer(f, store)
	h := s.Handler()
	state := startLogin(t, s, "upstox")
	call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	delete(store.rows, "upstox")
	call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	if len(store.rows) != 0 {
		t.Error("state worked twice")
	}

	state = startLogin(t, s, "upstox")
	s.Now = func() time.Time { return clock().Add(pendingTTL + time.Second) }
	call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	if len(store.rows) != 0 {
		t.Error("expired state worked")
	}
}

func TestPastedToken(t *testing.T) {
	ctx := context.Background()
	t.Run("bare zerodha token", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		startLogin(t, s, "zerodha")
		if err := s.Paste(ctx, "zerodha", "  reqtoken\n"); err != nil {
			t.Fatal(err)
		}
		if f.form.Get("checksum") != kiteChecksum || store.rows["zerodha"].UserID != "ZD1" || !s.Done("zerodha") {
			t.Errorf("checksum %q rows %v", f.form.Get("checksum"), store.rows)
		}
	})
	t.Run("bare upstox code", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		startLogin(t, s, "upstox")
		s.Paste(ctx, "upstox", "abc")
		if f.form.Get("code") != "abc" || store.rows["upstox"].UserID != "UPX1" {
			t.Errorf("code %q rows %v", f.form.Get("code"), store.rows)
		}
	})
	t.Run("whole redirected URL", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		state := startLogin(t, s, "zerodha")
		err := s.Paste(ctx, "zerodha", "https://example.com/cb?action=login&status=success&request_token=reqtoken&state="+state)
		if err != nil || f.form.Get("checksum") != kiteChecksum {
			t.Fatal(err)
		}
	})
	t.Run("redirected URL without state", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		startLogin(t, s, "zerodha")
		if err := s.Paste(ctx, "zerodha", "https://example.com/cb?request_token=reqtoken"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("URL from another login is refused", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		startLogin(t, s, "zerodha")
		if err := s.Paste(ctx, "zerodha", "https://example.com/cb?request_token=x&state=forged"); err != ErrNoLogin {
			t.Fatal(err)
		}
		if len(store.rows) != 0 {
			t.Error("stored a token")
		}
	})
	t.Run("URL without the token", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		startLogin(t, s, "zerodha")
		if err := s.Paste(ctx, "zerodha", "https://example.com/cb?status=cancelled"); err == nil || !strings.Contains(err.Error(), "no request_token") {
			t.Fatal(err)
		}
	})
	t.Run("needs a login in progress", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		if err := s.Paste(ctx, "zerodha", "reqtoken"); err != ErrNoLogin {
			t.Fatal(err)
		}
		startLogin(t, s, "upstox")
		if err := s.Paste(ctx, "zerodha", "reqtoken"); err != ErrNoLogin {
			t.Fatalf("other broker's login accepted: %v", err)
		}
	})
	t.Run("login expires", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		startLogin(t, s, "zerodha")
		s.Now = func() time.Time { return clock().Add(pendingTTL + time.Second) }
		if err := s.Paste(ctx, "zerodha", "reqtoken"); err != ErrNoLogin {
			t.Fatal(err)
		}
	})
	t.Run("refused token can be retried, then login closes", func(t *testing.T) {
		f, store := newFake(t), newMem()
		s := newServer(f, store)
		startLogin(t, s, "zerodha")
		f.exchange = 403
		err := s.Paste(ctx, "zerodha", "typo")
		if err == nil || strings.Contains(err.Error(), "typo") || len(store.rows) != 0 {
			t.Fatalf("%v %v", err, store.rows)
		}
		f.exchange = 200
		if err := s.Paste(ctx, "zerodha", "reqtoken"); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if err := s.Paste(ctx, "zerodha", "reqtoken"); err != ErrNoLogin {
			t.Fatalf("login stayed open: %v", err)
		}
	})
}

func TestGuards(t *testing.T) {
	f, store := newFake(t), newMem()
	s := newServer(f, store)
	h := s.Handler()
	state := startLogin(t, s, "upstox")
	if w := call(h, "GET", "/callback/upstox?code=abc&state="+state, map[string]string{"Host": "evil.example"}); w.Code != http.StatusForbidden {
		t.Errorf("rebinding host on callback: %d", w.Code)
	}
	if len(store.rows) != 0 {
		t.Error("a blocked request had an effect")
	}
	// The page and its assets, nothing else of the build (the manual page's index.html)
	s.UI = fstest.MapFS{"login.html": {Data: []byte("<p>logins</p>")}, "index.html": {Data: []byte("manual")},
		"assets/a.js": {Data: []byte("js")}}
	h = s.Handler()
	for path, want := range map[string]int{"/": 200, "/login.html": 200, "/assets/a.js": 200, "/index.html": 404, "/x": 404} {
		if w := call(h, "GET", path, nil); w.Code != want {
			t.Errorf("GET %s: %d, want %d", path, w.Code, want)
		}
	}
	// The API answers only its own page's JSON posts
	json := map[string]string{"Origin": origin, "Content-Type": "application/json"}
	for name, c := range map[string]struct {
		headers map[string]string
		want    int
	}{
		"no origin":    {map[string]string{"Content-Type": "application/json"}, http.StatusForbidden},
		"other origin": {map[string]string{"Origin": "https://evil.example", "Content-Type": "application/json"}, http.StatusForbidden},
		"a form post":  {map[string]string{"Origin": origin, "Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		"rebinding":    {map[string]string{"Origin": origin, "Content-Type": "application/json", "Host": "evil.example"}, http.StatusForbidden},
		"its own page": {json, http.StatusOK},
	} {
		if w := post(h, "/api/brokers/upstox/login", "{}", c.headers); w.Code != c.want {
			t.Errorf("%s: %d, want %d", name, w.Code, c.want)
		}
	}
	if w := call(h, "GET", "/callback/upstox?code=abc&state="+state, nil); w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("cache %q", w.Header().Get("Cache-Control"))
	}
}

func TestLogout(t *testing.T) {
	ctx := context.Background()
	active := func() *memStore {
		s := newMem()
		s.rows["upstox"] = Token{Broker: "upstox", UserID: "U", AccessToken: realToken, ExpiresAt: clock().Add(time.Hour)}
		s.rows["zerodha"] = Token{Broker: "zerodha", UserID: "Z", AccessToken: realToken, ExpiresAt: clock().Add(time.Hour)}
		return s
	}
	t.Run("revokes then deletes", func(t *testing.T) {
		f, store := newFake(t), active()
		s := newServer(f, store)
		if err := Logout(ctx, store, s.Providers["upstox"], "upstox", clock()); err != nil {
			t.Fatal(err)
		}
		if f.bearer != "Bearer "+realToken || f.revoked != 1 {
			t.Errorf("bearer %q revoked %d", f.bearer, f.revoked)
		}
		if _, ok := store.rows["upstox"]; ok {
			t.Error("row kept")
		}
		Logout(ctx, store, s.Providers["zerodha"], "zerodha", clock())
		if f.deleteArgs.Get("access_token") != realToken || f.deleteArgs.Get("api_key") != "kitekey" {
			t.Errorf("kite args %v", f.deleteArgs)
		}
	})
	t.Run("failed revoke keeps the row", func(t *testing.T) {
		f, store := newFake(t), active()
		f.revoke = 500
		s := newServer(f, store)
		err := Logout(ctx, store, s.Providers["upstox"], "upstox", clock())
		if err == nil || !strings.Contains(err.Error(), "may still work") {
			t.Fatal(err)
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
		Logout(ctx, store, newServer(f, store).Providers["upstox"], "upstox", clock())
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
	state := startLogin(t, s, "upstox")
	w := call(h, "GET", "/callback/upstox?code=abc&state="+state, nil)
	seen.WriteString(w.Header().Get("Location") + w.Body.String())
	b, _ := json.Marshal(s.Statuses(context.Background()))
	seen.Write(b)
	f.exchange = 400
	state = startLogin(t, s, "upstox")
	seen.WriteString(call(h, "GET", "/callback/upstox?code=abc&state="+state, nil).Body.String())
	seen.WriteString(s.LastError("upstox"))

	tok := Token{Broker: "b", AccessToken: realToken}
	b, _ = json.Marshal(tok)
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
	os.WriteFile(path, []byte("# comment\n\nSERENE_T_A=one\nexport SERENE_T_B=\"two words\"\nSERENE_T_C='p=q'\nSERENE_T_SET=file\n"), 0o600)
	t.Setenv("SERENE_T_SET", "real")
	for _, k := range []string{"SERENE_T_A", "SERENE_T_B", "SERENE_T_C"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"SERENE_T_A": "one", "SERENE_T_B": "two words", "SERENE_T_C": "p=q", "SERENE_T_SET": "real"} {
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

func TestRunServesTheCallbackUntilCancelled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	f, store := newFake(t), newMem()
	s := newServer(f, store)
	s.Addr, s.Now = a, nil
	s.Providers["upstox"].(*Upstox).Now = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	state := startLogin(t, s, "upstox")
	var resp *http.Response
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for range 100 {
		if resp, err = client.Get("http://" + a + "/callback/upstox?code=x&state=" + state); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !s.Done("upstox") || len(store.rows) != 1 {
		t.Errorf("%d done=%v rows %v", resp.StatusCode, s.Done("upstox"), store.rows)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
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

func post(h http.Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://"+addr+path, strings.NewReader(body))
	r.Host = addr
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

// apiCall posts as the page does and returns its result, or fails the test.
func apiCall(t *testing.T, h http.Handler, path, body string) json.RawMessage {
	t.Helper()
	w := post(h, path, body, map[string]string{"Origin": origin, "Content-Type": "application/json"})
	var r struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || !r.OK {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body)
	}
	return r.Result
}

func TestThePageLogsInAndHandsTheLoginOn(t *testing.T) {
	f, store := newFake(t), newMem()
	s := newServer(f, store)
	handed := make(chan string, 4)
	s.After = func(_ context.Context, broker string, report func(Delivery)) {
		report(Delivery{To: "calm", State: "sending"})
		report(Delivery{To: "calm", State: "ok"})
		report(Delivery{To: "hearth", State: "failed", Error: "hearth isn't running"})
		handed <- broker
	}
	h := s.Handler()
	var start struct{ URL string }
	json.Unmarshal(apiCall(t, h, "/api/brokers/zerodha/login", "{}"), &start)
	if !strings.HasPrefix(start.URL, f.URL) {
		t.Fatalf("login url %q", start.URL)
	}
	if st := brokerState(t, s, "zerodha"); !st.Pending || st.Param != "request_token" {
		t.Fatalf("not pending: %+v", st)
	}
	apiCall(t, h, "/api/brokers/zerodha/token", `{"value":"https://example.com/cb?request_token=reqtoken"}`)
	if <-handed != "zerodha" {
		t.Fatal("not handed on")
	}
	s.Settle()
	st := brokerState(t, s, "zerodha")
	if st.State != "active" || st.Pending || st.IssuedAt == nil || len(st.Sent) != 2 ||
		st.Sent[0] != (Delivery{To: "calm", State: "ok"}) || st.Sent[1].State != "failed" {
		t.Fatalf("after login %+v", st)
	}
	// Sent again from the page; then logged out, which forgets where it went
	apiCall(t, h, "/api/brokers/zerodha/send", "{}")
	<-handed
	s.Settle()
	apiCall(t, h, "/api/brokers/zerodha/logout", "{}")
	if st := brokerState(t, s, "zerodha"); st.State != "logged_out" || len(st.Sent) != 0 || f.revoked == 0 {
		t.Fatalf("after logout %+v (revoked %d)", st, f.revoked)
	}
	// A broker's refusal comes back as the page's error, and the login stays open to paste again
	apiCall(t, h, "/api/brokers/upstox/login", "{}")
	f.exchange = 400
	w := post(h, "/api/brokers/upstox/token", `{"value":"abc"}`, map[string]string{"Origin": origin, "Content-Type": "application/json"})
	if !strings.Contains(w.Body.String(), `"ok":false`) || !brokerState(t, s, "upstox").Pending {
		t.Fatalf("refused paste: %s", w.Body)
	}
	apiCall(t, h, "/api/brokers/upstox/cancel", "{}")
	if brokerState(t, s, "upstox").Pending {
		t.Fatal("still pending after cancel")
	}
}
