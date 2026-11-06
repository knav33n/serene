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
)

// Status is what the page sees about a broker. It must never carry a token.
type Status struct {
	Name      string     `json:"name"`
	Label     string     `json:"label"`
	Param     string     `json:"token_param,omitempty"` // what the user pastes
	State     string     `json:"state"`                 // not_configured, logged_out, active, expired, error
	UserID    string     `json:"user_id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// pendingTTL allows for a manual login: 2FA, then copying the token back.
const pendingTTL = 10 * time.Minute

type pending struct {
	broker  string
	expires time.Time
}

// Server is the local login page and its API.
type Server struct {
	Addr      string // host:port, loopback only
	Providers map[string]Provider
	Store     Store
	UI        fs.FS
	Grace     time.Duration // wait after the last login, so the page can load
	Idle      time.Duration // stop after this long with no requests
	Now       func() time.Time

	mu        sync.Mutex
	pending   map[string]pending
	lastError map[string]string
	lastSeen  time.Time
	finished  chan struct{}
	finish    sync.Once
}

func (s *Server) origin() string { return "http://" + s.Addr }

// Handler returns the guarded routes.
func (s *Server) Handler() http.Handler {
	if s.finished == nil {
		s.finished = make(chan struct{})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/brokers", s.handleBrokers)
	mux.HandleFunc("POST /api/brokers/{name}/login", s.handleLogin)
	mux.HandleFunc("POST /api/brokers/{name}/token", s.handleToken)
	mux.HandleFunc("POST /api/brokers/{name}/logout", s.handleLogout)
	mux.HandleFunc("GET /callback/{name}", s.handleCallback)
	mux.HandleFunc("GET /", s.handleUI)
	return s.guard(mux)
}

// guard rejects anything not addressed to this server by its own page. The
// Host check stops DNS rebinding, and the Origin check stops another website
// from starting a login or logout in the background.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.Addr {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead
		if !safe && r.Header.Get("Origin") != s.origin() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.mu.Lock()
		s.lastSeen = now(s.Now)
		s.mu.Unlock()
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

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
			st.State = "expired"
			if t.Valid(now(s.Now)) {
				st.State = "active"
			}
		}
		if !configured && st.State == "logged_out" {
			st.State = "not_configured"
		}
		s.mu.Lock()
		if msg := s.lastError[b.Name]; msg != "" {
			st.Error = msg
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	return out
}

func (s *Server) handleBrokers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Statuses(r.Context()))
}

func (s *Server) setError(broker, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastError == nil {
		s.lastError = map[string]string{}
	}
	s.lastError[broker] = msg
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, ok := s.Providers[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": name + " is not configured"})
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start login"})
		return
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
	s.pending[state] = pending{name, at.Add(pendingTTL)}
	s.mu.Unlock()
	s.setError(name, "")
	writeJSON(w, http.StatusOK, map[string]string{"url": p.LoginURL(state)})
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

// handleToken finishes a login from a pasted token. The login stays open if
// the broker refuses, so a bad paste can be corrected. Origin is checked by
// guard, so only this page can reach here.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, ok := s.Providers[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": name + " is not configured"})
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	q, state, err := parsePaste(body.Value, p.TokenParam())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !s.pendingFor(name, state) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "This login has expired or wasn't started here. Click log in again."})
		return
	}
	t, err := p.Exchange(r.Context(), q)
	if err == nil {
		err = s.Store.Put(r.Context(), t)
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.clearPending(name)
	s.setError(name, "")
	writeJSON(w, http.StatusOK, s.Statuses(r.Context()))
	s.finishIfDone(r.Context())
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	back := func(result string) {
		q := url.Values{"broker": {name}, "result": {result}}
		http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
	}
	p, ok := s.Providers[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if !s.takeState(q.Get("state"), name) {
		s.setError(name, "This login didn't start from this page, or it took too long. Please try again.")
		back("failed")
		return
	}
	t, err := p.Exchange(r.Context(), q)
	if err != nil {
		s.setError(name, err.Error())
		back("failed")
		return
	}
	if err := s.Store.Put(r.Context(), t); err != nil {
		s.setError(name, err.Error())
		back("failed")
		return
	}
	s.setError(name, "")
	back("ok")
	s.finishIfDone(r.Context())
}

func (s *Server) finishIfDone(ctx context.Context) {
	for _, st := range s.Statuses(ctx) {
		if _, configured := s.Providers[st.Name]; configured && st.State != "active" {
			return
		}
	}
	s.finish.Do(func() {
		time.AfterFunc(s.Grace, func() { close(s.finished) })
	})
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

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := Logout(r.Context(), s.Store, s.Providers[name], name, now(s.Now)); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.setError(name, "")
	writeJSON(w, http.StatusOK, s.Statuses(r.Context()))
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[1:]
	if path != "" {
		if info, err := fs.Stat(s.UI, path); err == nil && !info.IsDir() {
			http.ServeFileFS(w, r, s.UI, path)
			return
		}
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(s.UI, "index.html")
	if err != nil {
		http.Error(w, "UI is not built: run npm run build in web/", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// Run serves until every configured broker is logged in, the page goes idle,
// or ctx ends.
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
	s.lastSeen = now(s.Now)
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for running := true; running; {
		select {
		case <-ctx.Done():
			running = false
		case <-s.finished:
			running = false
		case err := <-serveErr:
			return err
		case <-tick.C:
			s.mu.Lock()
			idle := now(s.Now).Sub(s.lastSeen)
			s.mu.Unlock()
			running = idle < s.Idle
		}
	}
	stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(stop)
}
