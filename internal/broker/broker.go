// Package broker defines how the engine reaches an execution venue, whether
// simulated or real. Each interface is small so that the simulator, paper and
// every live broker (Zerodha and Upstox) implement exactly the same contract;
// internal/broker/brokertest checks it.
package broker

import (
	"context"
	"errors"
	"time"

	"calm/internal/market"
	"calm/internal/oms"
)

// ErrNotOpen means a cancel named an order that is not open: unknown, or
// already filled, rejected or cancelled.
var ErrNotOpen = errors.New("order is not open")

// Broker executes orders.
type Broker interface {
	// Submit accepts an order. Fills arrive later through OnMinute.
	Submit(ctx context.Context, o market.Order) error
	// OnMinute advances to the minute starting at t and returns new fills.
	// bars holds that minute's candles by instrument key.
	OnMinute(ctx context.Context, t time.Time, bars map[string]market.Bar) ([]market.Fill, error)
	// Cancel withdraws an open order's unfilled legs. It returns ErrNotOpen
	// for an order that is not open.
	Cancel(ctx context.Context, orderID int64) error
	// EndOfDay cancels anything still open.
	EndOfDay(ctx context.Context) error
	// Pending counts orders that are open.
	Pending() int
}

// Position is a net position as the broker reports it.
type Position struct {
	Key string
	Qty int64 // signed: negative is short
}

// Account is what a broker says it holds, which reconciliation compares with
// calm's own books before trading.
type Account interface {
	// Positions returns non-zero net positions.
	Positions(ctx context.Context) ([]Position, error)
	// Orders returns today's orders with the state of each leg.
	Orders(ctx context.Context) ([]oms.Order, error)
}

// Funds reports money available for new positions.
type Funds interface {
	Available(ctx context.Context) (market.Paise, error)
}
