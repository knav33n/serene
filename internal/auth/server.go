package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"serene/internal/market"
)

// Status is what serene auth shows about a broker. It must never carry a token.
type Status struct {
	Name      string     `json:"name"`
	Label     string     `json:"label"`
	Param     string     `json:"token_param,omitempty"` // what the user pastes
	State     string     `json:"state"`                 // not_configured, logged_out, active, expired, error
	UserID    string     `json:"user_id,omitempty"`
	IssuedAt  *time.Time `json:"issued_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	Pending   bool       `json:"pending"`        // a login is open, waiting for its token
	Wait      string     `json:"wait,omitempty"` // why a login can't start now
	Sent      []Delivery `json:"sent,omitempty"` // where the last login was handed on
}

// Delivery is one place a login is handed on to (project_calm here, hearth).
type Delivery struct {
	To    string `json:"to"`
	State string `json:"state"` // sending, ok, failed
	Error string `json:"error,omitempty"`
}

// pendingTTL allows for a manual login: 2FA, then copying the token back.
const pendingTTL = 10 * time.Minute

type pending struct {
	broker  string
	expires time.Time
}

// Server is the login page on a loopback address: it hands out each broker's
// login URL, takes the token pasted back, and serves the brokers' redirects
// to /callback/{name} so a login can also finish by itself. There's no
// password, so it never listens beyond loopback; the Host check stops DNS
// rebinding, and the Origin and JSON checks stop another site's page posting
// to it. Tokens never reach the page.
type Server struct {
	Addr      string // host:port, loopback only
	Providers map[string]Provider
	Store     Store
	Now       func() time.Time
	UI        fs.FS // the built page; its login.html is served at /
	// After hands a new login on (project_calm, hearth), reporting each place
	// as it goes. It runs in the background, so the page shows its progress.
	After func(ctx context.Context, broker string, report func(Delivery))
	Log   func(format string, a ...any)

	mu        sync.Mutex
	pending   map[string]pending
	lastError map[string]string
	done      map[string]bool // logged in since Start
	sent      map[string][]Delivery
	base      context.Context // Run's, for the hand-ons
	handing   sync.WaitGroup
}

// Handler is the page, its API and the brokers' redirects.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback/{name}", s.handleCallback)
	mux.HandleFunc("POST /api/brokers", s.action(func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		return s.Statuses(ctx), nil
	}))
	mux.HandleFunc("POST /api/brokers/{name}/login", s.action(func(_ context.Context, name string, _ json.RawMessage) (any, error) {
		u, err := s.Start(name)
		return map[string]string{"url": u}, err
	}))
	mux.HandleFunc("POST /api/brokers/{name}/token", s.action(func(ctx context.Context, name string, body json.RawMessage) (any, error) {
		var r struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, errors.New("send {value}")
		}
		if err := s.Paste(ctx, name, r.Value); err != nil {
			return nil, err
		}
		return s.Statuses(ctx), nil
	}))
	mux.HandleFunc("POST /api/brokers/{name}/cancel", s.action(func(ctx context.Context, name string, _ json.RawMessage) (any, error) {
		s.clearPending(name)
		return s.Statuses(ctx), nil
	}))
	mux.HandleFunc("POST /api/brokers/{name}/logout", s.action(func(ctx context.Context, name string, _ json.RawMessage) (any, error) {
		if !known(name) {
			return nil, fmt.Errorf("unknown broker %q", name)
		}
		if err := Logout(ctx, s.Store, s.Providers[name], name, now(s.Now)); err != nil {
			return nil, err
		}
		s.mu.Lock()
		delete(s.sent, name)
		s.mu.Unlock()
		return s.Statuses(ctx), nil
	}))
	mux.HandleFunc("POST /api/brokers/{name}/send", s.action(func(ctx context.Context, name string, _ json.RawMessage) (any, error) {
		t, err := s.Store.Get(ctx, name)
		if err != nil || !t.Valid(now(s.Now)) {
			return nil, fmt.Errorf("%s has no login today to send", name)
		}
		s.handOn(name)
		return s.Statuses(ctx), nil
	}))
	mux.HandleFunc("GET /", s.page)
	return s.guard(mux)
}

func known(name string) bool {
	for _, b := range Brokers {
		if b.Name == name {
			return true
		}
	}
	return false
}

func (s *Server) origin() string { return "http://" + s.Addr }

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.Addr {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("Origin") != s.origin() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(ct) != "application/json" {
				http.Error(w, "JSON only", http.StatusUnsupportedMediaType)
				return
			}
		}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
			"font-src https://fonts.gstatic.com; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

// action answers {ok, result} or {ok: false, error}, as the page expects.
func (s *Server) action(fn func(ctx context.Context, name string, body json.RawMessage) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "a JSON object"})
			return
		}
		res, err := fn(r.Context(), r.PathValue("name"), body)
		if err != nil {
			s.logf("%s: %v", r.URL.Path, err)
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
	}
}

// page serves login.html at / and the page's assets.
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if s.UI == nil {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path != "" && path != "login.html" {
		if strings.HasPrefix(path, "assets/") || path == "favicon.svg" {
			if info, err := fs.Stat(s.UI, path); err == nil && !info.IsDir() {
				http.ServeFileFS(w, r, s.UI, path)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(s.UI, "login.html")
	if err != nil {
		http.Error(w, "the page isn't built: run npm run build in web/", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// handOn runs After for a broker in the background.
func (s *Server) handOn(broker string) {
	if s.After == nil {
		return
	}
	s.mu.Lock()
	if s.sent == nil {
		s.sent = map[string][]Delivery{}
	}
	s.sent[broker] = nil
	ctx := s.base
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	report := func(d Delivery) {
		s.mu.Lock()
		defer s.mu.Unlock()
		list := s.sent[broker]
		for i := range list {
			if list[i].To == d.To {
				list[i] = d
				return
			}
		}
		s.sent[broker] = append(list, d)
	}
	s.handing.Add(1)
	go func() {
		defer s.handing.Done()
		s.After(ctx, broker, report)
	}()
}

// Settle waits for the logins being handed on.
func (s *Server) Settle() { s.handing.Wait() }

// Statuses reports every broker.
func (s *Server) Statuses(ctx context.Context) []Status {
	out := make([]Status, 0, len(Brokers))
	for _, b := range Brokers {
		st := Status{Name: b.Name, Label: b.Label}
		p, configured := s.Providers[b.Name]
		if configured {
			st.Param = p.TokenParam()
		}
		t, err := s.Store.Get(ctx, b.Name)
		switch {
		case errors.Is(err, ErrNoToken):
			st.State = "logged_out"
		case err != nil:
			st.State, st.Error = "error", err.Error()
		default:
			st.UserID, st.ExpiresAt = t.UserID, &t.ExpiresAt
			if !t.IssuedAt.IsZero() {
				st.IssuedAt = &t.IssuedAt
			}
			st.State = "expired"
			if t.Valid(now(s.Now)) {
				st.State = "active"
			}
		}
		if !configured && st.State == "logged_out" {
			st.State = "not_configured"
		}
		if b.Name == "zerodha" && configured && TooEarly(now(s.Now)) {
			st.Wait = "Zerodha logs every token out after 06:00: log in after 07:30 IST"
		}
		st.Pending = s.pendingFor(b.Name, "")
		s.mu.Lock()
		if msg := s.lastError[b.Name]; msg != "" {
			st.Error = msg
		}
		st.Sent = append([]Delivery(nil), s.sent[b.Name]...)
		s.mu.Unlock()
		out = append(out, st)
	}
	return out
}

func (s *Server) setError(broker, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastError == nil {
		s.lastError = map[string]string{}
	}
	s.lastError[broker] = msg
}

// LastError is why the broker's last login failed, if it did.
func (s *Server) LastError(broker string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastError[broker]
}

func (s *Server) markDone(broker string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		s.done = map[string]bool{}
	}
	s.done[broker] = ok
}

// Done reports whether the broker has logged in since its Start.
func (s *Server) Done(broker string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done[broker]
}

// Start opens a login for the broker and returns the URL to log in at.
func (s *Server) Start(name string) (string, error) {
	p, ok := s.Providers[name]
	if !ok {
		return "", errors.New(name + " is not configured: add its API key and secret to .env")
	}
	if name == "zerodha" && TooEarly(now(s.Now)) {
		return "", errors.New("Zerodha's daily logout can still wipe a Kite login made between 06:00 and 07:30 IST: log in after 07:30")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("could not start login")
	}
	state := hex.EncodeToString(b)
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]pending{}
	}
	at := now(s.Now)
	for k, v := range s.pending {
		if at.After(v.expires) {
			delete(s.pending, k)
		}
	}
	for k, v := range s.pending {
		if v.broker == name { // one open login a broker: the newest
			delete(s.pending, k)
		}
	}
	s.pending[state] = pending{name, at.Add(pendingTTL)}
	s.mu.Unlock()
	s.setError(name, "")
	s.markDone(name, false)
	return p.LoginURL(state), nil
}

// TooEarly reports whether t is between 06:00 and 07:30 IST, when Zerodha's
// daily logout may still wipe a new Kite login (one made at 06:04 was dead by
// 09:00). project_calm doesn't count a Kite login from then.
func TooEarly(t time.Time) bool {
	m := t.In(market.IST).Hour()*60 + t.In(market.IST).Minute()
	return m >= 6*60 && m < 7*60+30
}

// takeState consumes a state value, so each one works exactly once.
func (s *Server) takeState(state, broker string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[state]
	if ok {
		delete(s.pending, state)
	}
	return ok && p.broker == broker && !now(s.Now).After(p.expires)
}

// parsePaste accepts a bare token or the whole redirected URL, and returns
// query values for Exchange plus any state the URL carried.
func parsePaste(value, param string) (url.Values, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, "", errors.New("paste the token first")
	}
	if !strings.Contains(value, "?") {
		return url.Values{param: {value}}, "", nil
	}
	u, err := url.Parse(value)
	if err != nil {
		return nil, "", errors.New("that doesn't look like a token or a URL")
	}
	q := u.Query()
	if q.Get(param) == "" {
		return nil, "", fmt.Errorf("that URL has no %s", param)
	}
	return q, q.Get("state"), nil
}

// pendingFor reports whether a login is open for a broker. A state, when the
// pasted URL has one, must be that login's.
func (s *Server) pendingFor(broker, state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := now(s.Now)
	for k, v := range s.pending {
		if v.broker == broker && !at.After(v.expires) && (state == "" || state == k) {
			return true
		}
	}
	return false
}

func (s *Server) clearPending(broker string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.pending {
		if v.broker == broker {
			delete(s.pending, k)
		}
	}
}

// ErrNoLogin is a paste with no login of its own open.
var ErrNoLogin = errors.New("this login has expired or wasn't started here: start it again")

// Paste finishes a login from a pasted token or redirected URL. The login
// stays open if the broker refuses, so a bad paste can be corrected.
func (s *Server) Paste(ctx context.Context, name, value string) error {
	p, ok := s.Providers[name]
	if !ok {
		return errors.New(name + " is not configured")
	}
	q, state, err := parsePaste(value, p.TokenParam())
	if err != nil {
		return err
	}
	if !s.pendingFor(name, state) {
		return ErrNoLogin
	}
	t, err := p.Exchange(ctx, q)
	if err == nil {
		err = s.Store.Put(ctx, t)
	}
	if err != nil {
		return err
	}
	s.clearPending(name)
	s.setError(name, "")
	s.markDone(name, true)
	s.handOn(name)
	return nil
}

// handleCallback finishes a login from the broker's redirect, then sends the
// tab to the page, which says how it went.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, ok := s.Providers[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := func(result string) { http.Redirect(w, r, "/?"+result+"="+url.QueryEscape(name), http.StatusSeeOther) }
	q := r.URL.Query()
	if !s.takeState(q.Get("state"), name) {
		s.setError(name, "this login didn't start here, or it took too long: log in again")
		back("failed")
		return
	}
	t, err := p.Exchange(r.Context(), q)
	if err == nil {
		err = s.Store.Put(r.Context(), t)
	}
	if err != nil {
		s.setError(name, err.Error())
		back("failed")
		return
	}
	s.setError(name, "")
	s.markDone(name, true)
	s.handOn(name)
	back("done")
}

// Logout revokes a token at the broker, then deletes it. If the revoke fails
// the token is kept, because it may still work and hiding it would mislead.
// An expired token only needs deleting.
func Logout(ctx context.Context, store Store, p Provider, broker string, at time.Time) error {
	t, err := store.Get(ctx, broker)
	if errors.Is(err, ErrNoToken) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.Valid(at) {
		if p == nil {
			return fmt.Errorf("%s has no API keys set, so its token can't be revoked", broker)
		}
		if err := p.Revoke(ctx, t); err != nil {
			return fmt.Errorf("token may still work: %w", err)
		}
	}
	return store.Delete(ctx, broker)
}

// Run serves the page and the brokers' redirects until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", s.Addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing to serve login on %q: use a loopback IP such as 127.0.0.1", host)
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(stop)
}
