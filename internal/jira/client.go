// Package jira is a minimal client for the parts of the Jira Server/Data
// Center REST API this CLI needs: confirming a Personal Access Token works,
// and resolving a person's display name to their Jira username via the same
// mention/assignee picker Jira's own UI uses for @-mention autocomplete.
// This is useful to slack-cli at orgs where a person's Jira username is also
// their Slack username — see cmd/jira.go and resolveUserID's fallback.
package jira

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds every call to the configured Jira host. Without it,
// a typo'd or unreachable hostname — plausible for a public tool pointed at
// an arbitrary org's Jira — would hang indefinitely rather than failing fast.
const requestTimeout = 15 * time.Second

// User is the subset of a Jira user record this CLI cares about. On Jira
// Server/Data Center, Name is the person's username.
type User struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

// Client talks to a Jira Server/Data Center instance using a Personal Access
// Token (bearer auth, supported since Jira 8.14).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "https://" + baseURL
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: requestTimeout},
	}
}

func (c *Client) get(path string) (*http.Response, error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	return c.http.Do(req)
}

func errorFromResponse(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("jira returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// WhoAmI validates the token and returns the authenticated user, used by
// `jira config` to confirm the credentials work.
func (c *Client) WhoAmI() (*User, error) {
	resp, err := c.get("/rest/api/2/myself")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errorFromResponse(resp)
	}
	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, err
	}
	return &u, nil
}

// SearchUser looks up users matching a display name via Jira's user picker —
// the endpoint behind Jira's own @-mention/assignee autocomplete, available
// to any authenticated user (no "browse users" admin permission needed).
// Results are ranked by Jira's own relevance, most relevant first.
func (c *Client) SearchUser(query string) ([]User, error) {
	resp, err := c.get("/rest/api/2/user/picker?query=" + url.QueryEscape(query) + "&maxResults=10")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errorFromResponse(resp)
	}
	var parsed struct {
		Users []User `json:"users"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed.Users, nil
}
