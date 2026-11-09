package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/max7866/slack-cli/internal/api"
	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
)

var sendFiles []string
var sendThreadTS string
var sendBroadcast bool
var sendName string
var sendDryRun bool
var sendConfirm bool

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
		"Reply inside a thread with -t/--thread, using a timestamp shown in\n" +
		"`messages read`/`messages thread` output (works on a thread anyone\n" +
		"started, not just your own). The same flag also starts a brand-new\n" +
		"thread on a message that has no replies yet — in Slack there's no\n" +
		"separate \"start a thread\" action, the first reply to a message's\n" +
		"ts is what creates its thread. Add --broadcast to also post the\n" +
		"reply to the channel:\n\n" +
		"  slack-cli send #general \"fixed it\" -t 1700000000.000100\n\n" +
		"Don't know their @handle? Use --name with their full name (as Jira\n" +
		"shows it, e.g. \"Last, First\") to resolve them via their Jira username\n" +
		"instead — requires `slack-cli jira config` first (a no-op unless\n" +
		"your org's Jira usernames match your Slack usernames):\n\n" +
		"  slack-cli send --name \"Doe, Jane\" \"got a minute?\"\n\n" +
		"Add --dry-run to resolve everything (Jira lookup, Slack matching,\n" +
		"the channel/DM/thread target) and print what would be sent, without\n" +
		"actually posting anything:\n\n" +
		"  slack-cli send --name \"Doe, Jane\" \"test\" --dry-run\n\n" +
		"Add --confirm to preview the same resolution and ask [y/N] before\n" +
		"actually sending — unlike --dry-run, answering yes really sends it:\n\n" +
		"  slack-cli send #general \"fixed it\" --confirm\n\n" +
		"@mentions inside the message text (not just the recipient) are\n" +
		"resolved against the Slack directory and rewritten into real tags\n" +
		"that actually notify someone \xe2\x80\x94 plain \"@name\" text in a message\n" +
		"does not. @here/@channel/@everyone work too:\n\n" +
		"  slack-cli send #general \"hey @ana can you take this\"",
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
		typed := message
		if message != "" {
			message = resolveMentions(client, wsName, message)
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

		if sendDryRun {
			fmt.Println("[dry run] nothing was sent")
			printRecipientPreview(target, typed, message, channelID, sendThreadTS, sendBroadcast, sendFiles)
			return nil
		}

		if sendConfirm {
			fmt.Println("About to send:")
			printRecipientPreview(target, typed, message, channelID, sendThreadTS, sendBroadcast, sendFiles)
			fmt.Print("Send this? [y/N] ")
			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.ToLower(strings.TrimSpace(answer)) != "y" {
				fmt.Println("Aborted — nothing was sent.")
				return nil
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

// printRecipientPreview reports exactly what `send` would do — recipient
// resolution (including the Jira username fallback and opening a DM, which already
// happened by this point) and the message/files/thread target. Shared by
// --dry-run (never sends) and --confirm (asks before sending for real).
func printRecipientPreview(target, typed, message, channelID, threadTS string, broadcast bool, files []string) {
	fmt.Printf("  recipient:    %s (resolved channel/DM ID: %s)\n", target, channelID)
	if typed != "" {
		fmt.Printf("  message:      %q\n", typed)
	}
	if message != typed {
		fmt.Printf("  with mentions: %q\n", message)
	}
	for _, f := range files {
		fmt.Printf("  file:         %s\n", f)
	}
	if threadTS != "" {
		fmt.Printf("  as reply in:  thread %s", threadTS)
		if broadcast {
			fmt.Print(" (also broadcast to channel)")
		}
		fmt.Println()
	}
}

func init() {
	sendCmd.Flags().StringArrayVarP(&sendFiles, "file", "f", nil, "Attach a file (repeatable)")
	sendCmd.Flags().StringVarP(&sendThreadTS, "thread", "t", "", "Reply within the thread rooted at this message timestamp")
	sendCmd.Flags().BoolVar(&sendBroadcast, "broadcast", false, "Also post the threaded reply to the channel (requires -t)")
	sendCmd.Flags().StringVar(&sendName, "name", "", "Resolve the recipient by full name via Jira instead of a #channel/@user positional arg")
	sendCmd.Flags().BoolVar(&sendDryRun, "dry-run", false, "Resolve the recipient and print what would be sent, without actually sending")
	sendCmd.Flags().BoolVar(&sendConfirm, "confirm", false, "Preview what would be sent and ask [y/N] before actually sending")
	rootCmd.AddCommand(sendCmd)
}
