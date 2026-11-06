// Package broker defines how the engine reaches an execution venue, whether
// simulated or real.
package broker

import (
	"context"
	"time"

	"calm/internal/market"
)

// Broker executes orders.
type Broker interface {
	// Submit accepts an order. Fills arrive later through OnMinute.
	Submit(ctx context.Context, o market.Order) error
	// OnMinute advances to the minute starting at t and returns new fills.
	// bars holds that minute's candles by instrument key.
	OnMinute(ctx context.Context, t time.Time, bars map[string]market.Bar) ([]market.Fill, error)
	// EndOfDay cancels anything still open.
	EndOfDay(ctx context.Context) error
	// Pending counts orders that are open.
	Pending() int
}
