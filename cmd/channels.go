package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/max7866/slack-cli/internal/api"
	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
)

var includeArchived bool
var mineOnly bool
var createPrivate bool
var createInvite []string

var channelsCmd = &cobra.Command{
	Use:   "channels",
	Short: "Manage channels",
}

var channelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List channels",
	RunE: func(cmd *cobra.Command, args []string) error {
		wsName, ws, err := loadWorkspace()
		if err != nil {
			return err
		}
		client := api.NewClient(ws)

		var channels []slack.Channel
		if includeArchived {
			// Archived channels are a different, rarely-needed set — always
			// fetch live rather than caching them alongside active channels.
			channels, err = fetchArchivedInclusive(client)
		} else {
			channels, err = getAllConversations(client, wsName, []string{"public_channel", "private_channel"})
		}
		if err != nil {
			return fmt.Errorf("failed to list channels: %w", err)
		}

		if mineOnly {
			resp, err := client.AuthTest()
			if err != nil {
				return fmt.Errorf("failed to resolve your user id: %w", err)
			}
			channels = filterByCreator(channels, resp.UserID)
		}

		printChannels(channels)
		return nil
	},
}

var channelsFavoritesCmd = &cobra.Command{
	Use:   "favorites",
	Short: "List your starred (favorite) channels",
	Long: "List the channels you've starred in Slack, so you can jump straight\n" +
		"to reading or replying in them without scanning the full channel list:\n\n" +
		"  slack-cli channels favorites\n" +
		"  slack-cli messages read #the-one-you-want",
	RunE: func(cmd *cobra.Command, args []string) error {
		wsName, ws, err := loadWorkspace()
		if err != nil {
			return err
		}
		client := api.NewClient(ws)

		starred, err := client.ListAllStars()
		if err != nil {
			return fmt.Errorf("failed to list starred items: %w", err)
		}

		starredIDs := make(map[string]bool)
		for _, item := range starred {
			if item.Type == "channel" {
				starredIDs[item.Channel] = true
			}
		}
		if len(starredIDs) == 0 {
			fmt.Println("No starred channels. Star one from Slack's sidebar first.")
			return nil
		}

		// Resolve names/topics from the cached directory (one fetch, usually
		// served from cache) rather than a live call per starred channel.
		channels, err := getAllConversations(client, wsName, []string{"public_channel", "private_channel"})
		if err != nil {
			return fmt.Errorf("failed to list channels: %w", err)
		}

		var favorites []slack.Channel
		for _, ch := range channels {
			if starredIDs[ch.ID] {
				favorites = append(favorites, ch)
			}
		}

		if len(favorites) < len(starredIDs) {
			fmt.Fprintf(os.Stderr, "note: %d starred channel(s) not shown — archived, or the cache predates them (try --refresh)\n",
				len(starredIDs)-len(favorites))
		}

		printChannels(favorites)
		return nil
	},
}

var channelsCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new channel",
	Long: "Create a new channel, optionally private, optionally inviting people\n" +
		"right away (each --invite value may be a single recipient or a\n" +
		"comma-separated list; the flag is also repeatable):\n\n" +
		"  slack-cli channels create project-x\n" +
		"  slack-cli channels create project-x --private\n" +
		"  slack-cli channels create project-x --private --invite @ana,@ben,carol@co.com",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsName, ws, err := loadWorkspace()
		if err != nil {
			return err
		}
		client := api.NewClient(ws)
		name := strings.TrimPrefix(args[0], "#")

		ch, err := client.CreateConversation(slack.CreateConversationParams{
			ChannelName: name,
			IsPrivate:   createPrivate,
		})
		if err != nil {
			return fmt.Errorf("failed to create channel: %w", err)
		}

		kind := "public"
		if createPrivate {
			kind = "private"
		}
		fmt.Printf("Created %s channel #%s (%s)\n", kind, ch.Name, ch.ID)

		// The channel list cache now predates this channel — drop it so the
		// next lookup fetches live instead of waiting out the long TTL.
		invalidateChannelCache(wsName)

		if len(createInvite) == 0 {
			return nil
		}

		var recipients []string
		for _, token := range createInvite {
			recipients = append(recipients, splitRecipients(token)...)
		}

		var ids []string
		var unresolved []string
		for _, r := range recipients {
			id, err := resolveUserID(client, wsName, r)
			if err != nil {
				unresolved = append(unresolved, r)
				continue
			}
			ids = append(ids, id)
		}

		if len(ids) > 0 {
			if _, err := client.InviteUsersToConversation(ch.ID, ids...); err != nil {
				return fmt.Errorf("channel created, but failed to invite people: %w", err)
			}
			fmt.Printf("Invited %d of %d requested\n", len(ids), len(recipients))
		}
		if len(unresolved) > 0 {
			fmt.Printf("Could not resolve (not invited): %s\n", strings.Join(unresolved, ", "))
		}
		return nil
	},
}

// filterByCreator returns the subset of channels created by the given user ID.
func filterByCreator(channels []slack.Channel, userID string) []slack.Channel {
	var mine []slack.Channel
	for _, ch := range channels {
		if ch.Creator == userID {
			mine = append(mine, ch)
		}
	}
	return mine
}

func printChannels(channels []slack.Channel) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTOPIC\tPRIVATE")
	for _, ch := range channels {
		private := ""
		if ch.IsPrivate {
			private = "yes"
		}
		topic := ch.Topic.Value
		if len(topic) > 50 {
			topic = topic[:50] + "..."
		}
		fmt.Fprintf(w, "#%s\t%s\t%s\n", ch.Name, topic, private)
	}
	w.Flush()
}

func init() {
	channelsListCmd.Flags().BoolVarP(&includeArchived, "archived", "a", false, "Include archived channels")
	channelsListCmd.Flags().BoolVar(&mineOnly, "mine", false, "Only show channels you created")
	channelsCreateCmd.Flags().BoolVarP(&createPrivate, "private", "p", false, "Create a private channel instead of public")
	channelsCreateCmd.Flags().StringArrayVarP(&createInvite, "invite", "i", nil, "Invite a person (or comma-separated list); repeatable")
	channelsCmd.AddCommand(channelsListCmd)
	channelsCmd.AddCommand(channelsFavoritesCmd)
	channelsCmd.AddCommand(channelsCreateCmd)
	rootCmd.AddCommand(channelsCmd)
}
