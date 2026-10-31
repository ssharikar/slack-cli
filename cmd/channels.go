package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/max7866/slack-cli/internal/api"
	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
)

var includeArchived bool
var mineOnly bool

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
	channelsCmd.AddCommand(channelsListCmd)
	channelsCmd.AddCommand(channelsFavoritesCmd)
	rootCmd.AddCommand(channelsCmd)
}
