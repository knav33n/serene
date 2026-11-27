package kite

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"serene/internal/auth"
)

// TickerURL is Kite's market data websocket.
const TickerURL = "wss://ws.kite.trade"

// Tick is one tick. Of a tradable instrument it is Kite's full-mode
// snapshot; of an index only LTP, Open/High/Low/Close and (full mode)
// Exchange are set. Kite
// sends a snapshot about once a second per instrument while it trades, not
// every trade; LastQty and LastTrade are the latest trade's.
type Tick struct {
	Token           int64
	LTP             float64
	LastQty         int64
	Avg             float64
	Volume          int64
	BuyQty, SellQty int64
	Open, High      float64
	Low, Close      float64
	LastTrade       time.Time
	OI              int64
	Exchange        time.Time // the exchange's timestamp of the snapshot
	Bid, Ask        float64   // best of the depth
	BidQty, AskQty  int64
}

// Full-mode packet sizes of a tradable instrument: ltp, quote, full.
const (
	tickLTP   = 8
	tickQuote = 44
	tickFull  = 184
	// An index's quote and full packets (its ltp packet is tickLTP).
	tickIndexQuote = 28
	tickIndexFull  = 32
)

// ParseTicks decodes one binary message: a count, then each packet's length
// and bytes. A 1-byte heartbeat is no ticks.
// Packets of other sizes are skipped. Prices come in paise.
func ParseTicks(b []byte) ([]Tick, error) {
	if len(b) < 2 {
		return nil, nil
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	var out []Tick
	for range n {
		if len(b) < 2 {
			return out, fmt.Errorf("kite ticker: packet header cut short")
		}
		size := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+size {
			return out, fmt.Errorf("kite ticker: packet of %d bytes cut short", size)
		}
		p := b[2 : 2+size]
		b = b[2+size:]
		i32 := func(at int) int64 { return int64(int32(binary.BigEndian.Uint32(p[at:]))) }
		px := func(at int) float64 { return float64(i32(at)) / 100 }
		switch size {
		case tickLTP, tickQuote, tickFull:
		case tickIndexQuote, tickIndexFull:
			t := Tick{Token: i32(0), LTP: px(4), High: px(8), Low: px(12), Open: px(16), Close: px(20)}
			if size == tickIndexFull {
				if s := i32(28); s > 0 {
					t.Exchange = time.Unix(s, 0)
				}
			}
			out = append(out, t)
			continue
		default:
			continue
		}
		t := Tick{Token: i32(0), LTP: px(4)}
		if size >= tickQuote {
			t.LastQty, t.Avg, t.Volume = i32(8), px(12), i32(16)
			t.BuyQty, t.SellQty = i32(20), i32(24)
			t.Open, t.High, t.Low, t.Close = px(28), px(32), px(36), px(40)
		}
		if size == tickFull {
			if s := i32(44); s > 0 {
				t.LastTrade = time.Unix(s, 0)
			}
			t.OI = i32(48)
			if s := i32(60); s > 0 {
				t.Exchange = time.Unix(s, 0)
			}
			// Depth: 5 bids then 5 asks of qty int32, price int32, orders
			// int16 and 2 bytes of padding.
			t.BidQty, t.Bid = i32(64), px(68)
			t.AskQty, t.Ask = i32(64+5*12), px(68+5*12)
		}
		out = append(out, t)
	}
	return out, nil
}

// Ticker is one websocket connection to Kite's market data.
type Ticker struct{ conn *websocket.Conn }

// DialTicker connects to base (TickerURL) with the day's access token.
func DialTicker(ctx context.Context, base, apiKey string, token auth.Secret) (*Ticker, error) {
	u := base + "?" + url.Values{"api_key": {apiKey}, "access_token": {string(token)}}.Encode()
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dctx, u, nil)
	if err != nil {
		if resp != nil && (resp.StatusCode == 401 || resp.StatusCode == 403) {
			return nil, ErrAuth
		}
		// The error can carry the URL, and so the token: say less.
		return nil, fmt.Errorf("kite ticker: dial failed (%T)", err)
	}
	conn.SetReadLimit(1 << 22)
	return &Ticker{conn: conn}, nil
}

// Subscribe asks for full-mode ticks of tokens.
func (t *Ticker) Subscribe(ctx context.Context, tokens []int64) error {
	if len(tokens) == 0 {
		return nil
	}
	for _, m := range []any{
		map[string]any{"a": "subscribe", "v": tokens},
		map[string]any{"a": "mode", "v": []any{"full", tokens}},
	} {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if err := t.conn.Write(ctx, websocket.MessageText, b); err != nil {
			return fmt.Errorf("kite ticker: subscribe: %w", err)
		}
	}
	return nil
}

// Read waits for the next message and returns its ticks: none for a
// heartbeat or a text message, an error for Kite's error message.
// Cancelling ctx closes the connection.
func (t *Ticker) Read(ctx context.Context) ([]Tick, error) {
	typ, b, err := t.conn.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("kite ticker: %w", err)
	}
	if typ == websocket.MessageText {
		var m struct {
			Type string `json:"type"`
			Data any    `json:"data"`
		}
		if json.Unmarshal(b, &m) == nil && m.Type == "error" {
			return nil, fmt.Errorf("kite ticker: %v", m.Data)
		}
		return nil, nil
	}
	return ParseTicks(b)
}

// Close ends the connection.
func (t *Ticker) Close() error { return t.conn.Close(websocket.StatusNormalClosure, "") }
