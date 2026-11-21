// Package slack sends messages and files through a Slack app.
//
// The bot token needs chat:write and files:write, plus im:write to send direct
// messages. The bot must be a member of a private channel.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client calls the Slack Web API.
type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

// New returns a client. The token comes from the environment.
func New(token string) *Client {
	return &Client{Token: token, BaseURL: "https://slack.com/api", HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The error text holds only the URL, never the header.
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack: HTTP %d", resp.StatusCode)
	}
	var status struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Needed string `json:"needed"`
		Detail struct {
			Messages []string `json:"messages"`
		} `json:"response_metadata"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		return fmt.Errorf("slack: unreadable response: %w", err)
	}
	if !status.OK {
		if status.Needed != "" {
			return fmt.Errorf("slack: %s: add %s to the app and reinstall it", status.Error, status.Needed)
		}
		if len(status.Detail.Messages) > 0 {
			return fmt.Errorf("slack: %s: %s", status.Error, strings.Join(status.Detail.Messages, "; "))
		}
		return fmt.Errorf("slack: %s", status.Error)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return c.do(req, out)
}

// OpenDM returns the channel for a direct message with a user.
func (c *Client) OpenDM(ctx context.Context, user string) (string, error) {
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	err := c.call(ctx, "conversations.open", map[string]string{"users": user}, &out)
	return out.Channel.ID, err
}

// Resolve turns a channel setting into a conversation ID. A member ID, which
// starts with U or W, becomes a direct message. Anything else must be a channel
// ID, as Slack's file sharing doesn't accept channel names.
func (c *Client) Resolve(ctx context.Context, target string) (string, error) {
	if strings.HasPrefix(target, "U") || strings.HasPrefix(target, "W") {
		return c.OpenDM(ctx, target)
	}
	if strings.HasPrefix(target, "#") {
		return "", fmt.Errorf("SLACK_CHANNEL must be a channel ID such as C0123ABC, not %s", target)
	}
	return target, nil
}

// Post sends a message.
func (c *Client) Post(ctx context.Context, channel, text string) error {
	return c.call(ctx, "chat.postMessage", map[string]string{"channel": channel, "text": text}, nil)
}

// PostBlocks sends a message laid out with Block Kit blocks; text is the
// notification and fallback. It returns the message's ts.
func (c *Client) PostBlocks(ctx context.Context, channel, text string, blocks []Block) (string, error) {
	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", map[string]any{"channel": channel, "text": text, "blocks": blocks}, &out)
	return out.TS, err
}

// Reply posts text in the thread under the message ts.
func (c *Client) Reply(ctx context.Context, channel, ts, text string) error {
	return c.call(ctx, "chat.postMessage", map[string]string{"channel": channel, "thread_ts": ts, "text": text}, nil)
}

// maxAlt is Slack's limit on alt text.
const maxAlt = 1000

// File is one attachment. Alt describes an image for screen readers.
type File struct {
	Path, Title, Alt string
}

// Upload posts files as one message with a comment. An image shows inline.
// Slack's flow is to ask for an upload URL per file, send the bytes there, then
// complete them together, which shares them.
func (c *Client) Upload(ctx context.Context, channel, comment string, files []File) error {
	refs := make([]map[string]string, 0, len(files))
	for _, f := range files {
		id, err := c.send(ctx, f)
		if err != nil {
			return err
		}
		refs = append(refs, map[string]string{"id": id, "title": f.Title})
	}
	return c.call(ctx, "files.completeUploadExternal", map[string]any{
		"channel_id": channel, "initial_comment": comment, "files": refs,
	}, nil)
}

// send uploads one file's bytes and returns its ID. The token doesn't go to the
// upload URL, which is already signed.
func (c *Client) send(ctx context.Context, f File) (string, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", errors.New("slack: empty file")
	}
	q := url.Values{"filename": {filepath.Base(f.Path)}, "length": {strconv.Itoa(len(b))}}
	if f.Alt != "" {
		alt := []rune(f.Alt)
		q.Set("alt_txt", string(alt[:min(len(alt), maxAlt)]))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/files.getUploadURLExternal?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	var ticket struct {
		URL string `json:"upload_url"`
		ID  string `json:"file_id"`
	}
	if err := c.do(req, &ticket); err != nil {
		return "", err
	}
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, ticket.URL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	up.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTP.Do(up)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("slack: upload: HTTP %d", resp.StatusCode)
	}
	return ticket.ID, nil
}
