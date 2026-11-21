package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Block is one Block Kit block, as Slack's JSON.
type Block = map[string]any

// Button is an interactive button. Value comes back in the click's payload;
// Confirm, when set, is asked in a dialog before the click is sent.
type Button struct {
	ActionID, Text, Value string
	Style                 string // "", "primary" (green) or "danger" (red)
	Confirm               string
}

// SectionBlock is a block of mrkdwn text.
func SectionBlock(text string) Block {
	return Block{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": text}}
}

// ActionsBlock is a row of buttons.
func ActionsBlock(buttons ...Button) Block {
	els := make([]map[string]any, 0, len(buttons))
	for _, b := range buttons {
		el := map[string]any{"type": "button", "action_id": b.ActionID, "value": b.Value,
			"text": map[string]any{"type": "plain_text", "text": b.Text, "emoji": true}}
		if b.Style != "" {
			el["style"] = b.Style
		}
		if b.Confirm != "" {
			el["confirm"] = map[string]any{
				"title":   map[string]string{"type": "plain_text", "text": "Place a real order?"},
				"text":    map[string]string{"type": "mrkdwn", "text": b.Confirm},
				"confirm": map[string]string{"type": "plain_text", "text": "Place it"},
				"deny":    map[string]string{"type": "plain_text", "text": "Cancel"},
				"style":   "danger",
			}
		}
		els = append(els, el)
	}
	return Block{"type": "actions", "elements": els}
}

// MaxSkew is how old a signed request may be before it's refused as a replay.
const MaxSkew = 5 * time.Minute

// Verify checks a request from Slack against the app's signing secret:
// X-Slack-Signature is v0= and the hex HMAC-SHA256 of "v0:<timestamp>:<body>",
// and X-Slack-Request-Timestamp must be within MaxSkew of now.
func Verify(secret, timestamp, signature string, body []byte, now time.Time) error {
	if secret == "" {
		return errors.New("no signing secret")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("no request timestamp")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > MaxSkew || d < -MaxSkew {
		return fmt.Errorf("request timestamp is %s off", d.Round(time.Second))
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:", timestamp)
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return errors.New("bad signature")
	}
	return nil
}

// Click is one button press from an interactive message.
type Click struct {
	User, UserName string
	Channel        string
	MessageTS      string // the message the button is on
	ActionID       string
	Value          string
}

// ParseClick reads a block_actions payload, the form body Slack posts to the
// app's interactivity URL.
func ParseClick(body []byte) (Click, error) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return Click{}, err
	}
	var p struct {
		Type string `json:"type"`
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
		Container struct {
			MessageTS string `json:"message_ts"`
		} `json:"container"`
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(form.Get("payload")), &p); err != nil {
		return Click{}, fmt.Errorf("unreadable payload: %w", err)
	}
	if p.Type != "block_actions" || len(p.Actions) != 1 {
		return Click{}, fmt.Errorf("not one button press (%s, %d actions)", p.Type, len(p.Actions))
	}
	return Click{User: p.User.ID, UserName: p.User.Username, Channel: p.Channel.ID, MessageTS: p.Container.MessageTS,
		ActionID: p.Actions[0].ActionID, Value: p.Actions[0].Value}, nil
}
