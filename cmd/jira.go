package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/max7866/slack-cli/internal/config"
	"github.com/max7866/slack-cli/internal/jira"
	"github.com/spf13/cobra"
)

var jiraCmd = &cobra.Command{
	Use:   "jira",
	Short: "Manage the Jira integration used to resolve names to EUA IDs",
}

var jiraLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Save Jira credentials",
	Long: "Connects slack-cli to Jira so `send --name` and @name lookups can\n" +
		"resolve a person's name to their EUA ID (used as their Slack username\n" +
		"at this org) when the Slack directory doesn't have an exact match.\n\n" +
		"Needs a Personal Access Token: in Jira, click your avatar -> Personal\n" +
		"Access Tokens -> Create token.",
	RunE: func(cmd *cobra.Command, args []string) error {
		reader := bufio.NewReader(os.Stdin)

		fmt.Print("Jira base URL (e.g. https://jiraent.cms.gov): ")
		baseURL, _ := reader.ReadString('\n')
		baseURL = strings.TrimSpace(baseURL)
		baseURL = strings.TrimSuffix(baseURL, "/")
		if baseURL == "" {
			return fmt.Errorf("base URL is required")
		}

		fmt.Print("Personal Access Token: ")
		token, _ := reader.ReadString('\n')
		token = strings.TrimSpace(token)
		if token == "" {
			return fmt.Errorf("token is required")
		}

		client := jira.NewClient(baseURL, token)
		fmt.Println("\nValidating credentials...")
		me, err := client.WhoAmI()
		if err != nil {
			return fmt.Errorf("jira auth failed: %w", err)
		}

		if err := config.SaveJira(&config.JiraConfig{BaseURL: baseURL, Token: token}); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("\nConnected to Jira as %s (%s)\n", me.DisplayName, me.Name)
		fmt.Println("Saved to ~/.slack-cli/config.json")
		return nil
	},
}

var jiraLookupCmd = &cobra.Command{
	Use:   "lookup <name>",
	Short: "Look up a person's EUA ID (Jira username) by name",
	Long: "Look up a person's EUA ID by name — useful to sanity-check the\n" +
		"mapping before relying on `send --name` to find them automatically:\n\n" +
		"  slack-cli jira lookup \"Sharikar, Srikanth\"",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		jcfg, err := config.LoadJira()
		if err != nil {
			return err
		}
		client := jira.NewClient(jcfg.BaseURL, jcfg.Token)
		users, err := client.SearchUser(args[0])
		if err != nil {
			return fmt.Errorf("jira lookup failed: %w", err)
		}
		if len(users) == 0 {
			return fmt.Errorf("no Jira user matching %q", args[0])
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "EUA ID\tNAME")
		for _, u := range users {
			fmt.Fprintf(w, "%s\t%s\n", u.Name, u.DisplayName)
		}
		w.Flush()
		return nil
	},
}

func init() {
	jiraCmd.AddCommand(jiraLoginCmd)
	jiraCmd.AddCommand(jiraLookupCmd)
	rootCmd.AddCommand(jiraCmd)
}
