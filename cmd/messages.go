package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/max7866/slack-cli/internal/api"
	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
)

var messageCount int

var messagesCmd = &cobra.Command{
	Use:   "messages",
	Short: "Read messages",
}

var messagesReadCmd = &cobra.Command{
	Use:   "read [#channel | @user | @a,@b,@c]",
	Short: "Read messages from a channel, DM, or group DM",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsName, ws, err := loadWorkspace()
		if err != nil {
			return err
		}
		client := api.NewClient(ws)
		target := args[0]

		channelID, err := resolveTarget(client, wsName, target)
		if err != nil {
			return err
		}

		params := slack.GetConversationHistoryParameters{
			ChannelID: channelID,
			Limit:     messageCount,
		}
		history, err := client.GetConversationHistory(&params)
		if err != nil {
			return fmt.Errorf("failed to get messages: %w", err)
		}

		// Resolve display names from the cached workspace directory (one fetch,
		// usually served from cache) instead of a live users.info call per
		// message author — that used to make reading a busy channel very slow.
		userByID := make(map[string]slack.User)
		if users, err := getAllUsers(client, wsName); err == nil {
			for _, u := range users {
				userByID[u.ID] = u
			}
		}

		// Print messages in chronological order (oldest first)
		for i := len(history.Messages) - 1; i >= 0; i-- {
			msg := history.Messages[i]
			username := resolveUsername(client, msg.User, userByID)
			ts := formatTimestamp(msg.Timestamp)
			fmt.Fprintf(os.Stdout, "[%s] %s: %s\n", ts, username, msg.Text)
		}
		return nil
	},
}

func resolveTarget(client *slack.Client, wsName, target string) (string, error) {
	// A comma-separated list of people opens a multi-person DM (group DM), e.g.
	// "@ana,@ben,carol@co.com". Slack creates it with the same conversations.open
	// call used for a 1:1 DM — just with more user IDs.
	if strings.Contains(target, ",") {
		return resolveGroupDM(client, wsName, splitRecipients(target))
	}

	// A single email or @user opens a 1:1 DM.
	if strings.HasPrefix(target, "@") || isEmail(target) {
		id, err := resolveUserID(client, wsName, target)
		if err != nil {
			return "", err
		}
		return openDM(client, id)
	}

	// A raw conversation ID is passed through untouched.
	if isChannelID(target) {
		return target, nil
	}

	// Anything else is treated as a channel name (with or without a leading
	// '#'), resolved against the cached channel list — the same logic used by
	// `send`, so a channel you can send to can also be read from.
	return resolveChannelName(client, wsName, target)
}

// openDM opens (or reuses) a direct-message conversation with the given user IDs
// and returns its channel ID. One ID yields a 1:1 DM; several yield a group DM.
func openDM(client *slack.Client, userIDs ...string) (string, error) {
	ch, _, _, err := client.OpenConversation(&slack.OpenConversationParameters{
		Users: userIDs,
	})
	if err != nil {
		return "", fmt.Errorf("failed to open DM: %w", err)
	}
	return ch.ID, nil
}

// resolveGroupDM resolves each recipient to a user ID and opens a group DM with
// all of them.
func resolveGroupDM(client *slack.Client, wsName string, recipients []string) (string, error) {
	var ids []string
	seen := make(map[string]bool)
	for _, r := range recipients {
		id, err := resolveUserID(client, wsName, r)
		if err != nil {
			return "", err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) < 2 {
		return "", fmt.Errorf("a group DM needs at least 2 distinct people")
	}
	// Slack caps a group DM at 8 people besides yourself.
	if len(ids) > 8 {
		return "", fmt.Errorf("group DMs support at most 8 people (got %d)", len(ids))
	}
	return openDM(client, ids...)
}

// resolveUsername returns a display name for userID, preferring the cached
// workspace directory and falling back to a live users.info call only when
// the directory fetch failed or the user predates the cache.
func resolveUsername(client *slack.Client, userID string, userByID map[string]slack.User) string {
	if userID == "" {
		return "bot"
	}
	if u, ok := userByID[userID]; ok {
		if u.RealName != "" {
			return u.RealName
		}
		return u.Name
	}
	info, err := client.GetUserInfo(userID)
	if err != nil {
		return userID
	}
	name := info.RealName
	if name == "" {
		name = info.Name
	}
	userByID[userID] = *info
	return name
}

func formatTimestamp(ts string) string {
	parts := strings.Split(ts, ".")
	if len(parts) == 0 {
		return ts
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return ts
	}
	t := time.Unix(sec, 0)
	return t.Format("Jan 02 15:04")
}

func init() {
	messagesReadCmd.Flags().IntVarP(&messageCount, "count", "n", 20, "Number of messages to show")
	messagesCmd.AddCommand(messagesReadCmd)
	rootCmd.AddCommand(messagesCmd)
}
