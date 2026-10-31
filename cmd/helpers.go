package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/max7866/slack-cli/internal/config"
	"github.com/max7866/slack-cli/internal/jira"
	"github.com/slack-go/slack"
)

// dirCacheTTL is how long a cached user/channel directory is considered fresh.
// Resolving a recipient previously re-fetched the entire (paginated, rate-limited)
// directory on every send; caching it makes repeated sends effectively instant.
const dirCacheTTL = 10 * time.Minute

// refreshFlag, when set via --refresh, forces a live re-fetch and ignores any
// cached directory. Declared in root.go.

// loadWorkspace loads the selected workspace's credentials along with its
// resolved name (used as the cache key), honoring the global -w flag.
func loadWorkspace() (string, *config.Workspace, error) {
	name, err := config.ResolveName(workspaceFlag)
	if err != nil {
		return "", nil, err
	}
	ws, err := config.Load(workspaceFlag)
	if err != nil {
		return "", nil, err
	}
	return name, ws, nil
}

type cacheEnvelope struct {
	FetchedAt time.Time       `json:"fetched_at"`
	Payload   json.RawMessage `json:"payload"`
}

func cacheFile(key string) (string, error) {
	dir, err := config.CacheDir()
	if err != nil {
		return "", err
	}
	// key is a workspace name plus a suffix; keep the filename filesystem-safe.
	safe := strings.NewReplacer("/", "_", "\\", "_", " ", "_").Replace(key)
	return filepath.Join(dir, safe+".json"), nil
}

// loadCache decodes a fresh (< dirCacheTTL) cache entry into out. It returns
// false if caching is disabled (--refresh), the file is missing, stale, or
// unreadable — in which case the caller should fetch live.
func loadCache(key string, out any) bool {
	if refreshFlag {
		return false
	}
	path, err := cacheFile(key)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var env cacheEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return false
	}
	if time.Since(env.FetchedAt) > dirCacheTTL {
		return false
	}
	return json.Unmarshal(env.Payload, out) == nil
}

// saveCache writes v to the cache under key. Failures are non-fatal — caching is
// a performance optimization, not a source of truth.
func saveCache(key string, v any) {
	path, err := cacheFile(key)
	if err != nil {
		return
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return
	}
	env := cacheEnvelope{FetchedAt: time.Now(), Payload: payload}
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0600)
}

// getAllConversations returns every active conversation of the given types,
// following Slack's pagination cursor. Results are cached per workspace so that
// resolving a channel name doesn't re-scan the whole workspace on every command.
func getAllConversations(client *slack.Client, wsName string, types []string) ([]slack.Channel, error) {
	key := wsName + "-channels"
	var cached []slack.Channel
	if loadCache(key, &cached) {
		return cached, nil
	}
	all, err := fetchConversations(client, types, true)
	if err != nil {
		return nil, err
	}
	saveCache(key, all)
	return all, nil
}

// fetchArchivedInclusive fetches all public and private channels including
// archived ones, live (uncached).
func fetchArchivedInclusive(client *slack.Client) ([]slack.Channel, error) {
	return fetchConversations(client, []string{"public_channel", "private_channel"}, false)
}

// fetchConversations pages through conversations.list, following the cursor and
// backing off on rate limits. This is the raw, uncached fetch.
func fetchConversations(client *slack.Client, types []string, excludeArchived bool) ([]slack.Channel, error) {
	var all []slack.Channel
	cursor := ""
	for {
		params := &slack.GetConversationsParameters{
			Types:           types,
			Limit:           1000, // Slack still paginates below this; we follow the cursor.
			ExcludeArchived: excludeArchived,
			Cursor:          cursor,
		}
		channels, next, err := client.GetConversations(params)
		if err != nil {
			var rl *slack.RateLimitedError
			if errors.As(err, &rl) {
				time.Sleep(rl.RetryAfter)
				continue
			}
			return nil, err
		}
		all = append(all, channels...)
		if next == "" {
			break
		}
		cursor = next
	}
	return all, nil
}

// cachedUserByID returns the workspace's user directory keyed by ID, served
// from cache when fresh. Used to resolve message authors without a live
// users.info call per message (see resolveUsername).
func cachedUserByID(client *slack.Client, wsName string) map[string]slack.User {
	userByID := make(map[string]slack.User)
	if users, err := getAllUsers(client, wsName); err == nil {
		for _, u := range users {
			userByID[u.ID] = u
		}
	}
	return userByID
}

// fetchThreadReplies pages through conversations.replies for a thread,
// following the cursor and backing off on rate limits, so a long thread is
// read in full rather than truncated to the first page.
func fetchThreadReplies(client *slack.Client, channelID, threadTS string) ([]slack.Message, error) {
	var all []slack.Message
	cursor := ""
	for {
		params := &slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Limit:     200,
			Cursor:    cursor,
		}
		msgs, hasMore, next, err := client.GetConversationReplies(params)
		if err != nil {
			var rl *slack.RateLimitedError
			if errors.As(err, &rl) {
				time.Sleep(rl.RetryAfter)
				continue
			}
			return nil, err
		}
		all = append(all, msgs...)
		if !hasMore || next == "" {
			break
		}
		cursor = next
	}
	return all, nil
}

// resolveChannelName resolves a channel reference (with or without a leading
// '#') to a channel ID using the cached, fully-paginated conversation list.
func resolveChannelName(client *slack.Client, wsName, name string) (string, error) {
	name = strings.TrimPrefix(name, "#")
	types := []string{"public_channel", "private_channel"}

	channels, err := getAllConversations(client, wsName, types)
	if err != nil {
		return "", fmt.Errorf("failed to list channels: %w", err)
	}
	if id, ok := findChannel(channels, name); ok {
		return id, nil
	}

	// A miss may just mean the cache predates the channel — refetch live once.
	if !refreshFlag {
		channels, err = fetchConversations(client, types, true)
		if err != nil {
			return "", fmt.Errorf("failed to list channels: %w", err)
		}
		saveCache(wsName+"-channels", channels)
		if id, ok := findChannel(channels, name); ok {
			return id, nil
		}
	}
	return "", fmt.Errorf("channel #%s not found", name)
}

func findChannel(channels []slack.Channel, name string) (string, bool) {
	for _, ch := range channels {
		if ch.Name == name {
			return ch.ID, true
		}
	}
	return "", false
}

// findUser matches case-insensitively — Slack handles are case-insensitive in
// practice, and failing an exact-case match just sent people to `users search`
// to find the right casing, which is most of what made "finding a user" slow.
func findUser(users []slack.User, name string) (string, bool) {
	for _, u := range users {
		if strings.EqualFold(u.Name, name) || strings.EqualFold(u.RealName, name) || strings.EqualFold(u.Profile.DisplayName, name) {
			return u.ID, true
		}
	}
	return "", false
}

// resolveUserID resolves a single recipient token — an email, an "@handle", or a
// bare name/display name — to a Slack user ID. Emails use the direct lookup;
// everything else matches against the cached directory, refetching live once on a
// miss so a stale cache never yields a false "not found".
func resolveUserID(client *slack.Client, wsName, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("empty recipient")
	}
	if isEmail(token) {
		u, err := client.GetUserByEmail(token)
		if err != nil {
			return "", fmt.Errorf("user %q not found", token)
		}
		return u.ID, nil
	}

	name := strings.TrimPrefix(token, "@")
	users, err := getAllUsers(client, wsName)
	if err != nil {
		return "", fmt.Errorf("failed to list users: %w", err)
	}
	if id, ok := findUser(users, name); ok {
		return id, nil
	}
	if !refreshFlag {
		users, err = fetchUsersLive(client, wsName)
		if err != nil {
			return "", fmt.Errorf("failed to list users: %w", err)
		}
		if id, ok := findUser(users, name); ok {
			return id, nil
		}
	}

	// Last resort: resolve the name to an EUA ID via Jira (if connected) and
	// match that against the Slack directory. EUA ID is this org's Slack
	// username, and it catches names that don't match Slack's display name
	// verbatim — e.g. Jira's "Last, First (Org)" format vs whatever Slack shows.
	if euaID, ok := lookupEUAID(name); ok {
		if id, ok := findUser(users, euaID); ok {
			return id, nil
		}
	}

	return "", fmt.Errorf("user %q not found", token)
}

// lookupEUAID resolves a display name to an EUA ID via Jira's user picker.
// It returns false (not an error) whenever Jira isn't configured, the lookup
// comes back empty, or it's ambiguous — this is a best-effort fallback, not
// a hard dependency, so a user without Jira set up sees the same "not found"
// as before rather than a confusing Jira error. Ambiguous matches (common
// first names routinely return a dozen people at this org) are refused
// rather than silently guessing the first one — that would risk messaging a
// complete stranger.
func lookupEUAID(name string) (string, bool) {
	jcfg, err := config.LoadJira()
	if err != nil {
		return "", false
	}
	client := jira.NewClient(jcfg.BaseURL, jcfg.Token)
	matches, err := client.SearchUser(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: Jira lookup for %q failed: %v\n", name, err)
		return "", false
	}
	if len(matches) == 0 {
		return "", false
	}
	if len(matches) > 1 {
		fmt.Fprintf(os.Stderr, "warning: Jira matched %d people for %q — not auto-resolving an ambiguous name; run `jira lookup %q` to see candidates and use the exact full name (or their EUA ID directly)\n",
			len(matches), name, name)
		return "", false
	}
	return matches[0].Name, true
}

// isEmail reports whether token looks like an email address (and not an
// "@handle"), making it eligible for the users.lookupByEmail fast-path.
func isEmail(token string) bool {
	return !strings.HasPrefix(token, "@") &&
		strings.Contains(token, "@") &&
		strings.Contains(token, ".")
}

// splitRecipients splits a comma-separated recipient list into trimmed,
// non-empty tokens.
func splitRecipients(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isChannelID reports whether s looks like a raw Slack conversation ID
// (e.g. C0123ABCD for public, G… for private/group, D… for DMs) rather than a
// human-typed channel name.
func isChannelID(s string) bool {
	if len(s) < 8 {
		return false
	}
	switch s[0] {
	case 'C', 'G', 'D':
	default:
		return false
	}
	for _, r := range s[1:] {
		if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// getAllUsers returns every user in the workspace, served from a per-workspace
// cache when fresh. GetUsers already follows pagination internally; the cache
// avoids repeating that (rate-limited) scan on every send.
func getAllUsers(client *slack.Client, wsName string) ([]slack.User, error) {
	var cached []slack.User
	if loadCache(wsName+"-users", &cached) {
		return cached, nil
	}
	return fetchUsersLive(client, wsName)
}

// fetchUsersLive fetches the user directory from Slack (bypassing the cache) and
// refreshes the cache. 1000 (Slack's documented max) rather than 200 per page
// is a deliberate choice, not the default — measured live against a ~12.7k
// user workspace, requesting 200/page took 6m18s (64 requests), 1000/page
// took 1m9s (13 requests) for the identical result. The bottleneck here is
// per-request overhead (rate-limit backoff, network latency), not data
// volume, so fewer/larger pages wins decisively.
func fetchUsersLive(client *slack.Client, wsName string) ([]slack.User, error) {
	users, err := client.GetUsers(slack.GetUsersOptionLimit(1000))
	if err != nil {
		return nil, err
	}
	saveCache(wsName+"-users", users)
	return users, nil
}
