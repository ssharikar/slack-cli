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
	Short: "Optional Jira integration for resolving names to Jira usernames",
}

var jiraConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Save Jira connection details",
	Long: "Connects slack-cli to a Jira Server/Data Center instance so\n" +
		"`send --name` and @name lookups can resolve a person's name to their\n" +
		"Jira username when the Slack directory doesn't have an exact match.\n" +
		"This only helps at orgs where a person's Jira username is also their\n" +
		"Slack username — it's a no-op fallback everywhere else.\n\n" +
		"Needs a Personal Access Token: in Jira, click your avatar -> Personal\n" +
		"Access Tokens -> Create token.",
	RunE: func(cmd *cobra.Command, args []string) error {
		reader := bufio.NewReader(os.Stdin)

		fmt.Print("Jira base URL (e.g. https://jira.example.com): ")
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
	Short: "Look up a person's Jira username by name",
	Long: "Look up a person's Jira username by name — useful to sanity-check\n" +
		"the mapping before relying on `send --name` to find them automatically:\n\n" +
		"  slack-cli jira lookup \"Doe, Jane\"",
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
		fmt.Fprintln(w, "USERNAME\tNAME")
		for _, u := range users {
			fmt.Fprintf(w, "%s\t%s\n", u.Name, u.DisplayName)
		}
		w.Flush()
		return nil
	},
}

func init() {
	jiraCmd.AddCommand(jiraConfigCmd)
	jiraCmd.AddCommand(jiraLookupCmd)
	rootCmd.AddCommand(jiraCmd)
}
