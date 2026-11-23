package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"serene/internal/auth"
	"serene/internal/market"
	"serene/internal/slack"
)

// runSlackDelete deletes the app's own messages in a channel, thread replies
// included. Without --yes it only counts them. Messages by people or other
// apps can't be deleted with the bot token and are left alone.
func runSlackDelete(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("slack delete", flag.ContinueOnError)
	channel := fs.String("channel", "", "channel name (e.g. trades or #trades) or ID")
	yes := fs.Bool("yes", false, "delete them; without it, only count")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *channel == "" {
		return errors.New("--channel is required")
	}
	if err := auth.LoadDotEnv(".env"); err != nil {
		return err
	}
	token := os.Getenv("SLACK_TOKEN")
	if token == "" {
		return errors.New("needs SLACK_TOKEN")
	}
	sc := slack.New(token)
	me, err := sc.Whoami(ctx)
	if err != nil {
		return err
	}
	ch, err := sc.ChannelID(ctx, *channel)
	if err != nil {
		return err
	}
	mine := func(m slack.Message) bool {
		switch m.Subtype {
		case "channel_join", "channel_leave", "group_join", "group_leave":
			return false
		}
		return (me.BotID != "" && m.BotID == me.BotID) || (m.User != "" && m.User == me.UserID)
	}
	top, err := sc.History(ctx, ch)
	if err != nil {
		return err
	}
	// Replies first, so a deleted parent leaves no thread behind.
	var del []slack.Message
	others := 0
	for _, m := range top {
		if m.ReplyCount > 0 {
			rs, err := sc.Replies(ctx, ch, m.TS)
			if err != nil {
				return err
			}
			for _, r := range rs {
				if mine(r) {
					del = append(del, r)
				} else {
					others++
				}
			}
		}
	}
	nTop := 0
	for _, m := range top {
		if mine(m) {
			del = append(del, m)
			nTop++
		} else {
			others++
		}
	}
	fmt.Printf("%s (%s): %d of the app's messages (%d top-level, %d thread replies); %d by others, which stay\n",
		*channel, ch, len(del), nTop, len(del)-nTop, others)
	if !*yes {
		for _, m := range del[max(len(del)-5, 0):] {
			fmt.Printf("  %s  %.80s\n", tsTime(m.TS), m.Text)
		}
		if len(del) > 0 {
			fmt.Println("add --yes to delete them")
		}
		return nil
	}
	for i, m := range del {
		if err := sc.Delete(ctx, ch, m.TS); err != nil {
			return fmt.Errorf("deleted %d of %d, then %s: %w", i, len(del), tsTime(m.TS), err)
		}
		if (i+1)%50 == 0 {
			fmt.Printf("deleted %d of %d\n", i+1, len(del))
		}
	}
	fmt.Printf("deleted %d message(s)\n", len(del))
	return nil
}

// tsTime is a Slack ts as IST time.
func tsTime(ts string) string {
	var sec, frac int64
	fmt.Sscanf(ts, "%d.%d", &sec, &frac)
	return time.Unix(sec, 0).In(market.IST).Format("2006-01-02 15:04")
}
