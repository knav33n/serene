package manual

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"serene/internal/market"
)

// Server is the page and its API, on a loopback address only. Locally
// there's no password: anyone who can reach the port can place real orders,
// so it never listens beyond loopback. Served to the internet (Public, behind
// a proxy such as hearth's Caddy) the API needs the Gate's login first.
// The Host check stops DNS rebinding, and the Origin and JSON checks stop
// another website's page from posting to it.
type Server struct {
	Addr   string // host:port, loopback
	Public string // the host it's also served as, over HTTPS, e.g. hearth.naveenkumar.dev; needs Gate
	Gate   *Gate
	Desk   *Desk
	UI     fs.FS
	Log    func(format string, a ...any)
}

func (s *Server) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
	}
}

// Handler is the guarded routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", s.session)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		if s.Gate != nil {
			http.SetCookie(w, s.Gate.Logout())
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": nil})
	})
	mux.HandleFunc("POST /api/manual", s.action(func(ctx context.Context, _ json.RawMessage) (any, error) {
		return s.Desk.Overview(ctx)
	}))
	mux.HandleFunc("POST /api/manual/buy", s.action(func(ctx context.Context, body json.RawMessage) (any, error) {
		var r BuyReq
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("the buy: %w", err)
		}
		return s.Desk.Buy(ctx, r)
	}))
	mux.HandleFunc("POST /api/manual/exit", s.action(func(ctx context.Context, body json.RawMessage) (any, error) {
		var r struct{ Broker, Symbol, Exchange, Product string }
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("the exit: %w", err)
		}
		return s.Desk.Exit(ctx, r.Broker, r.Symbol, r.Exchange, r.Product)
	}))
	mux.HandleFunc("GET /", s.page)
	return s.guard(mux)
}

// origin is the page's own origin for the Host it was asked on.
func (s *Server) origin(host string) string {
	if s.Public != "" && host == s.Public {
		return "https://" + s.Public
	}
	return "http://" + s.Addr
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.Addr && (s.Public == "" || r.Host != s.Public) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("Origin") != s.origin(r.Host) {
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

// in is whether r may use the API: always without a Gate, else with its session.
func (s *Server) in(r *http.Request) bool { return s.Gate == nil || s.Gate.In(r) }

// session tells the page whether it must log in first.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": map[string]bool{"gated": s.Gate != nil, "in": s.in(r)}})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.Gate == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": nil})
		return
	}
	var body struct{ Password, Code string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "a JSON object"})
		return
	}
	c, err := s.Gate.Login(body.Password, body.Code)
	if err != nil {
		s.logf("login refused from %s: %v", clientIP(r), err)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.logf("logged in from %s", clientIP(r))
	http.SetCookie(w, c)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": nil})
}

// clientIP is who asked: Caddy's X-Forwarded-For when it's the proxy, else the peer.
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	return r.RemoteAddr
}

// action answers {ok, result} or {ok: false, error}, as the page expects;
// 401 without a session when the page is gated.
func (s *Server) action(fn func(ctx context.Context, body json.RawMessage) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.in(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "log in first"})
			return
		}
		var body json.RawMessage
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 20<<10)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "a JSON object"})
			return
		}
		res, err := fn(r.Context(), body)
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

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path != "" && path != "index.html" {
		if info, err := fs.Stat(s.UI, path); err == nil && !info.IsDir() {
			http.ServeFileFS(w, r, s.UI, path)
			return
		}
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(s.UI, "index.html")
	if err != nil {
		http.Error(w, "the page isn't built: run npm run build in web/", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// Run serves until ctx ends, squaring off the manual positions at 15:20 on
// each trading day it's running.
func (s *Server) Run(ctx context.Context) error {
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", s.Addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing to serve the manual page on %q: use a loopback IP such as 127.0.0.1 (a proxy serves it beyond)", host)
	}
	if s.Public != "" && (s.Gate == nil || s.Gate.PasswordHash == "" || len(s.Gate.TOTP) == 0 || len(s.Gate.Key) == 0) {
		return fmt.Errorf("served as %s, the page needs its login: a password hash, a TOTP secret and a session key", s.Public)
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	var squared market.Date
	for {
		select {
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return srv.Shutdown(stop)
		case err := <-done:
			return err
		case <-tick.C:
			now := s.Desk.now()
			d := market.DateOf(now)
			if squared == d || !market.NSETradingDay(d) || before(now, CloseAt) || !before(now, [2]int{15, 30}) {
				continue
			}
			squared = d
			for _, b := range Brokers {
				if err := s.Desk.SquareOff(ctx, b); err != nil {
					s.logf("15:20 square-off on %s: %v", b, err)
					s.Desk.say(ctx, ":rotating_light: manual: 15:20 square-off on %s: %v", Names[b], err)
				}
			}
		}
	}
}
