package manual

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestTOTPRFC6238(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, c := range []struct {
		unix int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		if got := TOTPCode(secret, c.unix/30); got != c.want {
			t.Errorf("%d: %s, want %s", c.unix, got, c.want)
		}
	}
	k, err := ParseTOTPSecret(strings.ToLower(NewTOTPSecret()))
	if err != nil || len(k) != 20 {
		t.Fatal(k, err)
	}
}

func TestPasswordHash(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("short password hashed")
	}
	h, err := HashPassword("correct horse battery")
	if err != nil || !CheckPassword(h, "correct horse battery") || CheckPassword(h, "correct horse batterY") || CheckPassword("junk", "x") {
		t.Fatal(h, err)
	}
}

type clockAt struct{ t time.Time }

func (c *clockAt) now() time.Time { return c.t }

func gate(t *testing.T) (*Gate, *clockAt) {
	t.Helper()
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	c := &clockAt{time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}
	return &Gate{PasswordHash: h, TOTP: []byte("12345678901234567890"), Key: []byte("k"), Secure: true, Now: c.now}, c
}

func (g *Gate) codeNow() string { return TOTPCode(g.TOTP, g.now().Unix()/30) }

func TestGateLogin(t *testing.T) {
	g, clock := gate(t)
	if _, err := g.Login("correct horse battery", "000000"); !errors.Is(err, ErrLogin) {
		t.Fatal("wrong code accepted", err)
	}
	c, err := g.Login("correct horse battery", g.codeNow())
	if err != nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatal(c, err)
	}
	if want := time.Date(2026, 11, 21, 0, 30, 0, 0, time.UTC); !c.Expires.Equal(want) { // 06:00 IST
		t.Errorf("expires %v", c.Expires)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	if !g.In(r) {
		t.Fatal("session refused")
	}
	if _, err := g.Login("correct horse battery", g.codeNow()); err == nil {
		t.Error("a code was taken twice")
	}
	tampered := *c
	tampered.Value = strings.Replace(c.Value, ".", "9.", 1)
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.AddCookie(&tampered)
	if g.In(r2) {
		t.Error("tampered cookie accepted")
	}
	clock.t = c.Expires
	if g.In(r) {
		t.Error("expired session accepted")
	}
	clock.t = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	g.PasswordHash, _ = HashPassword("another long password")
	if g.In(r) {
		t.Error("a new password kept the old sessions")
	}
}

func TestGateLocksOut(t *testing.T) {
	g, clock := gate(t)
	for range 5 {
		g.Login("nope", "123456")
	}
	var locked ErrLocked
	clock.t = clock.t.Add(31 * time.Second)
	if _, err := g.Login("correct horse battery", g.codeNow()); !errors.As(err, &locked) {
		t.Fatal("not locked", err)
	}
	clock.t = clock.t.Add(time.Minute)
	if _, err := g.Login("correct horse battery", g.codeNow()); err != nil {
		t.Fatal("still locked", err)
	}
}

func TestServerGated(t *testing.T) {
	g, _ := gate(t)
	z := &fake{name: "zerodha", funds: 1}
	s := &Server{Addr: "127.0.0.1:8767", Public: "hearth.example", Gate: g, Desk: desk(t, z),
		UI: fstest.MapFS{"index.html": {Data: []byte("<html>manual</html>")}}}
	h := s.Handler()
	call := func(path, body string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://hearth.example"+path, strings.NewReader(body))
		r.Host = "hearth.example"
		r.Header.Set("Origin", "https://hearth.example")
		r.Header.Set("Content-Type", "application/json")
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("/api/manual", "{}", nil); w.Code != 401 {
		t.Fatalf("overview without a session: %d", w.Code)
	}
	if w := call("/api/manual/exit", `{"broker":"zerodha","symbol":"A","exchange":"NFO","product":"MIS"}`, nil); w.Code != 401 || len(z.cancelled) != 0 {
		t.Fatalf("exit without a session: %d", w.Code)
	}
	if w := call("/api/session", "{}", nil); !strings.Contains(w.Body.String(), `"gated":true,"in":false`) {
		t.Fatalf("session: %s", w.Body)
	}
	if w := call("/api/login", `{"password":"correct horse battery","code":"000000"}`, nil); w.Code != 401 {
		t.Fatalf("bad login: %d", w.Code)
	}
	w := call("/api/login", `{"password":"correct horse battery","code":"`+g.codeNow()+`"}`, nil)
	cs := w.Result().Cookies()
	if w.Code != 200 || len(cs) != 1 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	if w := call("/api/manual", "{}", cs[0]); w.Code != 200 || !strings.Contains(w.Body.String(), `"expiries"`) {
		t.Fatalf("overview with a session: %d %s", w.Code, w.Body)
	}
	if err := (&Server{Addr: "127.0.0.1:0", Public: "hearth.example", Desk: &Desk{}}).Run(context.Background()); err == nil {
		t.Error("served publicly without a gate")
	}
}
