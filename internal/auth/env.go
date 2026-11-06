package auth

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// LoadDotEnv sets variables from a file, leaving any already set alone. A
// missing file is fine. Errors give the line number, never the line, because
// the file holds secrets.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("%s line %d: expected NAME=value", path, n)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// Brokers lists every broker in display order.
var Brokers = []struct{ Name, Label string }{{"upstox", "Upstox"}, {"zerodha", "Zerodha"}}

// upstoxRedirect is sent on every Upstox login and must exactly match the URI
// registered on the app. Zerodha takes its redirect from its console instead.
func upstoxRedirect(baseURL string) string {
	if v := os.Getenv("UPSTOX_REDIRECT_URI"); v != "" {
		return v
	}
	return baseURL + "/callback/upstox"
}

// ProvidersFromEnv builds a provider for each broker with keys set.
func ProvidersFromEnv(baseURL string) map[string]Provider {
	out := map[string]Provider{}
	if k, s := os.Getenv("UPSTOX_API_KEY"), os.Getenv("UPSTOX_API_SECRET"); k != "" && s != "" {
		out["upstox"] = &Upstox{APIKey: k, APISecret: Secret(s), RedirectURI: upstoxRedirect(baseURL)}
	}
	if k, s := os.Getenv("ZERODHA_API_KEY"), os.Getenv("ZERODHA_API_SECRET"); k != "" && s != "" {
		out["zerodha"] = &Zerodha{APIKey: k, APISecret: Secret(s)}
	}
	return out
}
