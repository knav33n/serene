package manual

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"serene/internal/market"
)

// Gate is the page's login when it's served beyond loopback: a password
// and a TOTP code (an authenticator app's 6 digits) give a signed session
// cookie that lasts until 06:00 IST. Failures lock the login out for longer
// each time; a code is taken once.
type Gate struct {
	PasswordHash string // HashPassword's
	TOTP         []byte // the authenticator's secret
	Key          []byte // signs sessions; changing it or the password ends them all
	Secure       bool   // cookies only over HTTPS
	Now          func() time.Time

	mu       sync.Mutex
	fails    int
	lockedTo time.Time
	lastStep int64
}

// CookieName is the session cookie's.
const CookieName = "serene_manual"

const pbkdf2Iter = 600_000

// HashPassword is a PBKDF2-SHA256 hash of pw: pbkdf2-sha256:iterations:salt:hash.
func HashPassword(pw string) (string, error) {
	if len(pw) < 12 {
		return "", errors.New("use a password of 12 characters or more")
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	sum, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	b := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256:%d:%s:%s", pbkdf2Iter, b.EncodeToString(salt), b.EncodeToString(sum)), nil
}

// CheckPassword is whether pw matches hash.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, ":")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	b := base64.RawStdEncoding
	salt, err1 := b.DecodeString(parts[2])
	want, err2 := b.DecodeString(parts[3])
	if err != nil || err1 != nil || err2 != nil || iter < 1 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret is a fresh 20-byte secret, base32 as authenticator apps take it.
func NewTOTPSecret() string {
	s := make([]byte, 20)
	rand.Read(s)
	return b32.EncodeToString(s)
}

// ParseTOTPSecret reads a base32 secret, spaces and case ignored.
func ParseTOTPSecret(s string) ([]byte, error) {
	s = strings.ToUpper(strings.ReplaceAll(strings.TrimRight(s, "="), " ", ""))
	k, err := b32.DecodeString(s)
	if err != nil || len(k) < 10 {
		return nil, errors.New("the TOTP secret isn't base32 of 10 bytes or more")
	}
	return k, nil
}

// TOTPURI is the otpauth:// link an authenticator app adds the secret from.
func TOTPURI(secret, account string) string {
	q := url.Values{"secret": {secret}, "issuer": {"serene"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + url.PathEscape("serene:"+account) + "?" + q.Encode()
}

// TOTPCode is RFC 6238's 6-digit code for a 30-second step.
func TOTPCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1_000_000)
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// ErrLocked is a login refused because of earlier failures.
type ErrLocked struct{ Until time.Time }

func (e ErrLocked) Error() string {
	return "too many tries: wait until " + e.Until.In(market.IST).Format("15:04:05")
}

// ErrLogin is a wrong password or code; which isn't said.
var ErrLogin = errors.New("wrong password or code")

// Login checks a password and code, and gives the session cookie.
func (g *Gate) Login(pw, code string) (*http.Cookie, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if now.Before(g.lockedTo) {
		return nil, ErrLocked{g.lockedTo}
	}
	okPw := CheckPassword(g.PasswordHash, pw) // always run, so timing doesn't tell
	step, okCode := g.code(strings.TrimSpace(code), now)
	if !okPw || !okCode {
		g.fails++
		if g.fails >= 5 {
			wait := time.Minute << min(g.fails-5, 4) // 1, 2, 4, 8, then 16 minutes
			g.lockedTo = now.Add(wait)
		}
		return nil, ErrLogin
	}
	g.fails, g.lastStep = 0, step
	return g.cookie(nextMorning(now)), nil
}

// code finds the step code matches, within a step either way and after the last one used.
func (g *Gate) code(code string, now time.Time) (int64, bool) {
	if len(code) != 6 {
		return 0, false
	}
	at := now.Unix() / 30
	for _, s := range []int64{at, at - 1, at + 1} {
		if s > g.lastStep && subtle.ConstantTimeCompare([]byte(TOTPCode(g.TOTP, s)), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// nextMorning is the next 06:00 IST, when the brokers' logins lapse too.
func nextMorning(now time.Time) time.Time {
	t := now.In(market.IST)
	at := time.Date(t.Year(), t.Month(), t.Day(), 6, 0, 0, 0, market.IST)
	if !at.After(t) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

func (g *Gate) sign(exp int64) string {
	m := hmac.New(sha256.New, g.Key)
	fmt.Fprintf(m, "manual-session|%s|%d", g.PasswordHash, exp)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (g *Gate) cookie(exp time.Time) *http.Cookie {
	e := exp.Unix()
	return &http.Cookie{Name: CookieName, Value: strconv.FormatInt(e, 10) + "." + g.sign(e), Path: "/", Expires: exp,
		HttpOnly: true, Secure: g.Secure, SameSite: http.SameSiteStrictMode}
}

// Logout is the cookie that clears the session.
func (g *Gate) Logout() *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: g.Secure,
		SameSite: http.SameSiteStrictMode}
}

// In is whether r carries a live session.
func (g *Gate) In(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	e, sig, ok := strings.Cut(c.Value, ".")
	exp, err := strconv.ParseInt(e, 10, 64)
	if !ok || err != nil || g.now().Unix() >= exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(g.sign(exp)))
}
