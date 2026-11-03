package cmd

import (
	"fmt"
	"os"
	"path/filepath"
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
		userByID := cachedUserByID(client, wsName)

		// Print messages in chronological order (oldest first)
		for i := len(history.Messages) - 1; i >= 0; i-- {
			printMessage(client, history.Messages[i], userByID, "")
		}
		return nil
	},
}

var messagesThreadCmd = &cobra.Command{
	Use:   "thread [#channel | @user | @a,@b,@c] [thread-ts]",
	Short: "Read a thread's root message and all its replies",
	Long: "Read a thread's root message and all its replies.\n\n" +
		"Get the thread-ts from `messages read` output: a message with replies\n" +
		"shows a \"(N replies — ts ...)\" hint carrying the timestamp to pass here:\n\n" +
		"  slack-cli messages thread #general 1700000000.000100",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsName, ws, err := loadWorkspace()
		if err != nil {
			return err
		}
		client := api.NewClient(ws)
		target := args[0]
		threadTS := args[1]

		channelID, err := resolveTarget(client, wsName, target)
		if err != nil {
			return err
		}

		replies, err := fetchThreadReplies(client, channelID, threadTS)
		if err != nil {
			return fmt.Errorf("failed to get thread replies: %w", err)
		}

		// Same cached-directory lookup as `messages read` — a thread with many
		// distinct participants shouldn't cost a live users.info call each.
		userByID := cachedUserByID(client, wsName)

		for _, msg := range replies {
			prefix := "  ↳ " // reply
			if msg.Timestamp == threadTS {
				prefix = "[root] "
			}
			printMessage(client, msg, userByID, prefix)
		}
		return nil
	},
}

var downloadOutDir string

var messagesDownloadCmd = &cobra.Command{
	Use:   "download [#channel | @user | @a,@b,@c] [ts]",
	Short: "Download the files/images attached to one message",
	Long: "Download every file attached to a single message, identified by the\n" +
		"ts shown in `messages read`/`messages thread` output (works for a\n" +
		"top-level message or a thread reply):\n\n" +
		"  slack-cli messages download #general 1700000000.000100\n" +
		"  slack-cli messages download #general 1700000000.000100 -o ~/Downloads",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsName, ws, err := loadWorkspace()
		if err != nil {
			return err
		}
		client := api.NewClient(ws)
		target := args[0]
		ts := args[1]

		channelID, err := resolveTarget(client, wsName, target)
		if err != nil {
			return err
		}

		msg, err := findMessage(client, channelID, ts)
		if err != nil {
			return err
		}
		if len(msg.Files) == 0 {
			fmt.Println("That message has no attached files.")
			return nil
		}

		if downloadOutDir != "" {
			if err := os.MkdirAll(downloadOutDir, 0755); err != nil {
				return fmt.Errorf("failed to create output directory: %w", err)
			}
		}

		for _, f := range msg.Files {
			downloadURL := f.URLPrivateDownload
			if downloadURL == "" {
				downloadURL = f.URLPrivate
			}
			if downloadURL == "" {
				fmt.Fprintf(os.Stderr, "skipping %q: no download URL (external/removed file?)\n", f.Name)
				continue
			}
			name := filepath.Base(f.Name)
			if name == "" || name == "." || name == "/" {
				name = f.ID
			}
			path := filepath.Join(downloadOutDir, name)
			out, err := os.Create(path)
			if err != nil {
				return fmt.Errorf("failed to create %q: %w", path, err)
			}
			err = client.GetFile(downloadURL, out)
			out.Close()
			if err != nil {
				os.Remove(path)
				return fmt.Errorf("failed to download %q: %w", f.Name, err)
			}
			fmt.Printf("Saved %s (%s)\n", path, formatBytes(f.Size))
		}
		return nil
	},
}

// findMessage locates a single message by its exact ts, trying channel
// history first (covers top-level messages, including thread roots) and
// falling back to the thread it belongs to (covers thread replies, which
// conversations.history never returns on their own). Slack's
// conversations.replies accepts the ts of any message in a thread as the
// anchor, not just the root, which is what makes the fallback work without
// already knowing the thread's root ts.
func findMessage(client *slack.Client, channelID, ts string) (*slack.Message, error) {
	history, err := client.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Oldest:    ts,
		Latest:    ts,
		Inclusive: true,
		Limit:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to look up message: %w", err)
	}
	if len(history.Messages) > 0 && history.Messages[0].Timestamp == ts {
		return &history.Messages[0], nil
	}

	replies, err := fetchThreadReplies(client, channelID, ts)
	if err != nil {
		return nil, fmt.Errorf("failed to look up message: %w", err)
	}
	for _, msg := range replies {
		if msg.Timestamp == ts {
			return &msg, nil
		}
	}
	return nil, fmt.Errorf("no message found with ts %q in that conversation", ts)
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

// printMessage writes one formatted message line, resolving the author from
// the cached directory and always appending the raw timestamp — that's what
// `send -t`/`messages thread` need, whether to reply within a thread that
// already exists (shown as "N replies") or to start a brand-new thread on a
// message that has none yet (the first reply to any ts starts one). Any
// attached files (images, PDFs, etc.) are listed too — their bytes aren't
// shown, but the message no longer looks silently empty when a file is the
// whole point; `messages download` fetches the actual content.
func printMessage(client *slack.Client, msg slack.Message, userByID map[string]slack.User, prefix string) {
	username := resolveUsername(client, msg.User, userByID)
	ts := formatTimestamp(msg.Timestamp)
	line := fmt.Sprintf("%s[%s] %s: %s", prefix, ts, username, msg.Text)
	if msg.ReplyCount > 0 {
		line += fmt.Sprintf("  (%d replies — ts %s)", msg.ReplyCount, msg.Timestamp)
	} else {
		line += fmt.Sprintf("  (ts %s)", msg.Timestamp)
	}
	fmt.Fprintln(os.Stdout, line)
	for _, f := range msg.Files {
		fmt.Fprintf(os.Stdout, "%s  📎 %s (%s, %s) — `messages download` with this ts to save it\n",
			prefix, f.Name, f.Mimetype, formatBytes(f.Size))
	}
}

// formatBytes renders a byte count as a short human-readable size.
func formatBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
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
	messagesDownloadCmd.Flags().StringVarP(&downloadOutDir, "output", "o", "", "Directory to save files into (default: current directory)")
	messagesCmd.AddCommand(messagesReadCmd)
	messagesCmd.AddCommand(messagesThreadCmd)
	messagesCmd.AddCommand(messagesDownloadCmd)
	rootCmd.AddCommand(messagesCmd)
}
