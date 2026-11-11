package auth

import (
	"testing"

	"serene/internal/market"
)

func TestFromCalm(t *testing.T) {
	d := market.Date{Year: 2026, Month: 11, Day: 11}
	login := d.At(8, 40)
	kite, ok := FromCalm(CalmSession{Broker: "kite", ClientID: "U1", AccessToken: "k", LoginAt: login})
	if !ok || kite.Broker != "zerodha" || !kite.ExpiresAt.Equal(d.AddDays(1).At(6, 0)) || kite.UserID != "U1" {
		t.Fatalf("kite: %+v %v", kite, ok)
	}
	up, ok := FromCalm(CalmSession{Broker: "upstox", AccessToken: "u", LoginAt: login})
	if !ok || up.Broker != "upstox" || !up.ExpiresAt.Equal(d.AddDays(1).At(3, 30)) {
		t.Fatalf("upstox: %+v %v", up, ok)
	}
	if _, ok := FromCalm(CalmSession{Broker: "breeze", LoginAt: login}); ok {
		t.Fatal("breeze is not a serene broker")
	}
}
