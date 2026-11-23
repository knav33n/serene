package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Reading and deleting a channel's messages needs channels:read and
// channels:history (groups:read and groups:history for a private channel).
// A bot token deletes only the bot's own messages.

// RateLimited is Slack's HTTP 429: try again After.
type RateLimited struct{ After time.Duration }

func (e *RateLimited) Error() string {
	return fmt.Sprintf("slack: rate limited, retry after %s", e.After)
}

// Message is one message of a channel's history or a thread.
type Message struct {
	TS         string `json:"ts"`
	ThreadTS   string `json:"thread_ts"`
	User       string `json:"user"`
	BotID      string `json:"bot_id"`
	Text       string `json:"text"`
	Subtype    string `json:"subtype"` // e.g. channel_join, which can't be deleted
	ReplyCount int    `json:"reply_count"`
}

// get calls a read method with form parameters, waiting out rate limits.
func (c *Client) get(ctx context.Context, method string, q url.Values, out any) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/"+method+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		err = c.do(req, out)
		var rl *RateLimited
		if !errors.As(err, &rl) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rl.After):
		}
	}
}

// Identity is who the token belongs to.
type Identity struct {
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
}

// Whoami is the token's own user and bot.
func (c *Client) Whoami(ctx context.Context) (Identity, error) {
	var id Identity
	err := c.get(ctx, "auth.test", url.Values{}, &id)
	return id, err
}

// ChannelID is the ID of the channel named name (with or without '#'). An
// ID such as C0123ABC is returned as it is.
func (c *Client) ChannelID(ctx context.Context, name string) (string, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "#")
	if name == "" {
		return "", errors.New("no channel name")
	}
	if isID(name) {
		return name, nil
	}
	q := url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {"1000"}}
	for {
		var r struct {
			Channels []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"channels"`
			Meta struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.get(ctx, "conversations.list", q, &r); err != nil {
			return "", err
		}
		for _, ch := range r.Channels {
			if ch.Name == name {
				return ch.ID, nil
			}
		}
		if r.Meta.Next == "" {
			return "", fmt.Errorf("no channel #%s that the app can see", name)
		}
		q.Set("cursor", r.Meta.Next)
	}
}

func isID(s string) bool {
	if len(s) < 9 || !strings.ContainsAny(s[:1], "CGD") {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// History is every top-level message of a channel, newest first.
func (c *Client) History(ctx context.Context, channel string) ([]Message, error) {
	return c.messages(ctx, "conversations.history", url.Values{"channel": {channel}, "limit": {"200"}})
}

// Replies is a thread's replies, without its parent.
func (c *Client) Replies(ctx context.Context, channel, ts string) ([]Message, error) {
	ms, err := c.messages(ctx, "conversations.replies", url.Values{"channel": {channel}, "ts": {ts}, "limit": {"200"}})
	var out []Message
	for _, m := range ms {
		if m.TS != ts {
			out = append(out, m)
		}
	}
	return out, err
}

func (c *Client) messages(ctx context.Context, method string, q url.Values) ([]Message, error) {
	var out []Message
	for {
		var r struct {
			Messages []Message `json:"messages"`
			Meta     struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.get(ctx, method, q, &r); err != nil {
			return out, err
		}
		out = append(out, r.Messages...)
		if r.Meta.Next == "" {
			return out, nil
		}
		q.Set("cursor", r.Meta.Next)
	}
}

// Delete deletes one message, waiting out rate limits.
func (c *Client) Delete(ctx context.Context, channel, ts string) error {
	for {
		err := c.call(ctx, "chat.delete", map[string]string{"channel": channel, "ts": ts}, nil)
		var rl *RateLimited
		if !errors.As(err, &rl) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rl.After):
		}
	}
}
