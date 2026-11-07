// Package oms tracks orders through their life at a broker. It is shared by
// the simulator, paper and live adapters, so every path reports order state
// the same way, whichever broker it reaches.
package oms

import (
	"fmt"

	"calm/internal/market"
)

// State is where one leg of an order stands.
type State string

const (
	Intent     State = "intent"      // decided, not yet sent
	Sent       State = "sent"        // sent, no answer yet
	Acked      State = "acked"       // accepted by the broker, resting
	PartFilled State = "part_filled" // some quantity executed
	Filled     State = "filled"
	Rejected   State = "rejected"
	Cancelled  State = "cancelled"
	// Unknown means the order was sent and its fate is not known, for example
	// after a timeout. It must be resolved by looking the order up at the
	// broker, never by sending it again.
	Unknown State = "unknown"
)

// Terminal reports whether nothing more can happen to a leg.
func (s State) Terminal() bool { return s == Filled || s == Rejected || s == Cancelled }

var next = map[State][]State{
	Intent:     {Sent, Rejected},
	Sent:       {Acked, PartFilled, Filled, Rejected, Cancelled, Unknown},
	Acked:      {PartFilled, Filled, Rejected, Cancelled, Unknown},
	PartFilled: {PartFilled, Filled, Cancelled, Unknown},
	Unknown:    {Acked, PartFilled, Filled, Rejected, Cancelled, Unknown},
}

// CanMove reports whether a leg may go from one state to another.
func CanMove(from, to State) bool {
	for _, s := range next[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Leg is one leg of an order and what has happened to it.
type Leg struct {
	market.Leg
	// BrokerID is the broker's own order ID, kept beside the client ID because
	// each broker is searched by its own.
	BrokerID string
	State    State
	Filled   int64 // quantity executed, in units
	Reason   string
}

// Order is an order with the state of each leg.
type Order struct {
	market.Order
	LegStates []Leg
}

// New starts tracking an order, with every leg at Intent.
func New(o market.Order) *Order {
	t := &Order{Order: o, LegStates: make([]Leg, len(o.Legs))}
	for i, l := range o.Legs {
		t.LegStates[i] = Leg{Leg: l, State: Intent}
	}
	return t
}

// Move changes a leg's state, refusing a change the lifecycle does not allow.
func (o *Order) Move(leg int, to State, reason string) error {
	if leg < 0 || leg >= len(o.LegStates) {
		return fmt.Errorf("order %s: no leg %d", o.ClientID, leg)
	}
	l := &o.LegStates[leg]
	if !CanMove(l.State, to) {
		return fmt.Errorf("order %s leg %d: cannot go from %s to %s", o.ClientID, leg, l.State, to)
	}
	l.State, l.Reason = to, reason
	return nil
}

// Fill records executed quantity on a leg and moves it to PartFilled or Filled.
func (o *Order) Fill(leg int, qty int64) error {
	if leg < 0 || leg >= len(o.LegStates) {
		return fmt.Errorf("order %s: no leg %d", o.ClientID, leg)
	}
	l := &o.LegStates[leg]
	want := l.Qty()
	if qty <= 0 || l.Filled+qty > want {
		return fmt.Errorf("order %s leg %d: fill of %d with %d of %d already filled", o.ClientID, leg, qty, l.Filled, want)
	}
	to := PartFilled
	if l.Filled+qty == want {
		to = Filled
	}
	if err := o.Move(leg, to, ""); err != nil {
		return err
	}
	l.Filled += qty
	return nil
}

// Done reports whether every leg is terminal.
func (o *Order) Done() bool {
	for _, l := range o.LegStates {
		if !l.State.Terminal() {
			return false
		}
	}
	return true
}

// Broken reports whether an order ended with some legs filled and others not,
// leaving a structure that was never meant to be held.
func (o *Order) Broken() bool {
	if !o.Done() {
		return false
	}
	filled := 0
	for _, l := range o.LegStates {
		if l.Filled > 0 {
			filled++
		}
	}
	return filled > 0 && filled < len(o.LegStates)
}

// MaxClientID is the longest client ID calm sends. Brokers cap order tags
// (Kite at 20 characters), so IDs stay within the tightest known limit.
const MaxClientID = 20

// ClientID builds a client ID from a run prefix and an order number. The
// prefix is letters and digits only, so the ID passes any broker's tag rules.
func ClientID(prefix string, n int64) (string, error) {
	id := fmt.Sprintf("%s%06d", prefix, n)
	if err := CheckClientID(id); err != nil {
		return "", err
	}
	return id, nil
}

// CheckClientID refuses an ID a broker might reject or truncate.
func CheckClientID(id string) error {
	if id == "" || len(id) > MaxClientID {
		return fmt.Errorf("client ID %q must be 1 to %d characters", id, MaxClientID)
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return fmt.Errorf("client ID %q must be letters and digits only", id)
		}
	}
	return nil
}
