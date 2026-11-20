package auth

import "testing"

func TestCalmBroker(t *testing.T) {
	for broker, want := range map[string]string{"zerodha": "kite", "upstox": "upstox", "breeze": "breeze"} {
		if got, ok := CalmBroker(broker); !ok || got != want {
			t.Errorf("%s: %q %v", broker, got, ok)
		}
	}
	if _, ok := CalmBroker("fyers"); ok {
		t.Error("fyers isn't a calm broker")
	}
}
