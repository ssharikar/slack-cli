package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/max7866/slack-cli/internal/api"
	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
)

var sendFiles []string
var sendThreadTS string
var sendBroadcast bool
var sendName string

var sendCmd = &cobra.Command{
	Use:   "send [#channel | @user | @a,@b,@c] [message]",
	Short: "Send a message to a channel, user, or group DM",
	Long: "Send a message to a channel, a single user, or a group DM.\n\n" +
		"Recipients may be a #channel, a single @user or email, or a\n" +
		"comma-separated list of people (up to 8) to open a group DM:\n\n" +
		"  slack-cli send #general \"hi team\"\n" +
		"  slack-cli send @ana \"hey\"\n" +
		"  slack-cli send @ana,@ben,carol@co.com \"lunch?\"\n\n" +
		"Attach one or more files with -f/--file. The message becomes the\n" +
		"caption; it is optional when at least one file is attached:\n\n" +
		"  slack-cli send #general \"see attached\" -f report.pdf\n" +
		"  slack-cli send @ana -f a.png -f b.png\n\n" +
		"Reply inside a thread with -t/--thread, using a timestamp from\n" +
		"`messages read` or `messages thread` (works on a thread anyone\n" +
		"started, not just your own). Add --broadcast to also post the\n" +
		"reply to the channel:\n\n" +
		"  slack-cli send #general \"fixed it\" -t 1700000000.000100\n\n" +
		"Don't know their @handle? Use --name with their full name (as Jira\n" +
		"shows it, e.g. \"Last, First\") to resolve them via Jira's EUA ID\n" +
		"instead — requires `slack-cli jira login` first:\n\n" +
		"  slack-cli send --name \"Sharikar, Srikanth\" \"got a minute?\"",
	Args: cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		wsName, ws, err := loadWorkspace()
		if err != nil {
			return err
		}
		client := api.NewClient(ws)

		var target, message string
		if sendName != "" {
			if len(args) != 1 {
				return fmt.Errorf("with --name, provide exactly one message argument")
			}
			message = args[0]
		} else {
			if len(args) == 0 {
				return fmt.Errorf("provide a recipient (#channel, @user, or --name)")
			}
			target = args[0]
			if len(args) > 1 {
				message = args[1]
			}
		}
		if message == "" && len(sendFiles) == 0 {
			return fmt.Errorf("provide a message and/or at least one --file")
		}
		if sendBroadcast && sendThreadTS == "" {
			return fmt.Errorf("--broadcast only applies to a threaded reply (use -t/--thread)")
		}

		var channelID string
		if sendName != "" {
			id, err := resolveUserID(client, wsName, sendName)
			if err != nil {
				return fmt.Errorf("could not resolve %q (checked Slack directory and Jira): %w", sendName, err)
			}
			channelID, err = openDM(client, id)
			if err != nil {
				return err
			}
			target = sendName
		} else {
			channelID, err = resolveTarget(client, wsName, target)
			if err != nil {
				return err
			}
		}

		if len(sendFiles) > 0 {
			if err := uploadFiles(client, channelID, message, sendThreadTS, sendFiles); err != nil {
				return err
			}
			fmt.Printf("Sent %d file(s) to %s\n", len(sendFiles), target)
			return nil
		}

		opts := []slack.MsgOption{slack.MsgOptionText(message, false)}
		if sendThreadTS != "" {
			opts = append(opts, slack.MsgOptionTS(sendThreadTS))
			if sendBroadcast {
				opts = append(opts, slack.MsgOptionBroadcast())
			}
		}
		_, _, err = client.PostMessage(channelID, opts...)
		if err != nil {
			return fmt.Errorf("failed to send message: %w", err)
		}
		fmt.Printf("Message sent to %s\n", target)
		return nil
	},
}

// uploadFiles uploads each file to the channel. The message (if any) is attached
// as the caption on the first file so it reads as a single "here's the file"
// post; remaining files upload without a repeated comment. threadTS, when set,
// posts the upload as a reply within that thread instead of top-level.
func uploadFiles(client *slack.Client, channelID, message, threadTS string, paths []string) error {
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("cannot read file %q: %w", path, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%q is a directory, not a file", path)
		}
		comment := ""
		if i == 0 {
			comment = message
		}
		_, err = client.UploadFileV2(slack.UploadFileV2Parameters{
			Channel:         channelID,
			File:            path,
			Filename:        filepath.Base(path),
			FileSize:        int(info.Size()),
			InitialComment:  comment,
			ThreadTimestamp: threadTS,
		})
		if err != nil {
			return fmt.Errorf("failed to upload %q: %w", path, err)
		}
	}
	return nil
}

func init() {
	sendCmd.Flags().StringArrayVarP(&sendFiles, "file", "f", nil, "Attach a file (repeatable)")
	sendCmd.Flags().StringVarP(&sendThreadTS, "thread", "t", "", "Reply within the thread rooted at this message timestamp")
	sendCmd.Flags().BoolVar(&sendBroadcast, "broadcast", false, "Also post the threaded reply to the channel (requires -t)")
	sendCmd.Flags().StringVar(&sendName, "name", "", "Resolve the recipient by full name via Jira instead of a #channel/@user positional arg")
	rootCmd.AddCommand(sendCmd)
}
