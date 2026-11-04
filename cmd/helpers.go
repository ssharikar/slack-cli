package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/max7866/slack-cli/internal/config"
	"github.com/max7866/slack-cli/internal/jira"
	"github.com/slack-go/slack"
)

// dirCacheTTL is how long a cached user/channel directory is considered fresh.
// A 10-minute TTL defeats its own purpose on a large org: measured live on a
// workspace with tens of thousands of users, a cold refetch takes on the
// order of a minute even at the optimized page size, and real usage rarely
// sends two messages within 10 minutes of each other — so nearly every send
// was silently eating that full re-fetch first. An org directory doesn't
// change minute to minute, and
// a cache miss on a genuinely new/renamed person still self-heals (see the
// one-time live refetch in resolveUserID/resolveChannelName), so a much
// longer TTL costs little in staleness and removes the "every send is slow"
// problem for the entire rest of the day. --refresh/`cache clear` remain the
// escape hatches for "I need this to be live right now."
const dirCacheTTL = 12 * time.Hour

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

// channelDirCache and userDirCache memoize the decoded directory in memory
// for the lifetime of one CLI invocation. Without this, resolving several
// recipients (e.g. a group DM) re-reads and re-JSON-decodes the on-disk
// cache file — which holds every user in the workspace — once per recipient.
var channelDirCache = map[string][]slack.Channel{}
var userDirCache = map[string][]slack.User{}

// invalidateChannelCache drops the in-memory and on-disk channel cache for a
// workspace. Used after creating a channel so it's visible to the next
// lookup immediately rather than waiting out dirCacheTTL.
func invalidateChannelCache(wsName string) {
	delete(channelDirCache, wsName+"-channels")
	if path, err := cacheFile(wsName + "-channels"); err == nil {
		_ = os.Remove(path)
	}
}

// getAllConversations returns every active conversation of the given types,
// following Slack's pagination cursor. Results are cached per workspace so that
// resolving a channel name doesn't re-scan the whole workspace on every command.
func getAllConversations(client *slack.Client, wsName string, types []string) ([]slack.Channel, error) {
	key := wsName + "-channels"
	if cached, ok := channelDirCache[key]; ok {
		return cached, nil
	}
	var cached []slack.Channel
	if loadCache(key, &cached) {
		channelDirCache[key] = cached
		return cached, nil
	}
	all, err := fetchConversations(client, types, true)
	if err != nil {
		return nil, err
	}
	saveCache(key, all)
	channelDirCache[key] = all
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
		channelDirCache[wsName+"-channels"] = channels
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

// mentionPattern matches an "@handle"-shaped token in message text: an '@'
// at a word boundary (so it doesn't catch the '@' inside an email address
// like name@domain.com, which is preceded by a word character) followed by
// a Slack-handle-shaped word.
var mentionPattern = regexp.MustCompile(`(^|[^\w@])@([A-Za-z][\w.\-]*)`)

// resolveMentions rewrites recognized @handle tokens in message text into
// real Slack mentions — <@USER_ID> for a person, or <!here>/<!channel>/
// <!everyone> for the broadcast forms — so they actually tag/notify someone.
// Plain "@name" text in a Slack message does NOT notify anyone; only this
// syntax does, which is easy to miss since the plain text looks identical
// once posted.
//
// A token that doesn't match a known user is left exactly as typed: most
// @-looking substrings in free text are false positives (a time like "@
// 3pm", a stray symbol), so silently leaving them alone is the safe
// default. This only checks the already-cached Slack directory — no live
// refetch, no Jira fallback — since an inline mention is expected to be an
// exact handle, and a message with several @-looking tokens shouldn't
// trigger several live lookups.
func resolveMentions(client *slack.Client, wsName, text string) string {
	users, _ := getAllUsers(client, wsName)
	return mentionPattern.ReplaceAllStringFunc(text, func(match string) string {
		sub := mentionPattern.FindStringSubmatch(match)
		prefix, handle := sub[1], sub[2]
		switch strings.ToLower(handle) {
		case "here":
			return prefix + "<!here>"
		case "channel":
			return prefix + "<!channel>"
		case "everyone":
			return prefix + "<!everyone>"
		}
		if id, ok := findUser(users, handle); ok {
			return prefix + "<@" + id + ">"
		}
		return match
	})
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

	// Last resort: optional plugin-style fallback. Resolve the name to a Jira
	// username via Jira's user picker (if connected — most installs won't
	// have this configured, and that's fine) and match that against the
	// Slack directory. This only helps at orgs where a person's Jira
	// username is also their Slack username, but where it applies it catches
	// names that don't match Slack's display name verbatim — e.g. a Jira
	// "Last, First" format vs whatever Slack shows.
	if jiraUsername, ok := lookupJiraUsername(name); ok {
		if id, ok := findUser(users, jiraUsername); ok {
			return id, nil
		}
	}

	return "", fmt.Errorf("user %q not found", token)
}

// lookupJiraUsername resolves a display name to a Jira username via Jira's
// user picker. It returns false (not an error) whenever Jira isn't
// configured, the lookup comes back empty, or it's ambiguous — this is a
// best-effort fallback, not a hard dependency, so a user without Jira set up
// sees the same "not found" as before rather than a confusing Jira error.
// Ambiguous matches (a common first name can easily return a dozen people at
// a large org) are refused rather than silently guessing the first one —
// that would risk messaging a complete stranger.
func lookupJiraUsername(name string) (string, bool) {
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
		fmt.Fprintf(os.Stderr, "warning: Jira matched %d people for %q — not auto-resolving an ambiguous name; run `jira lookup %q` to see candidates and use the exact full name (or their Jira/Slack username directly)\n",
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
	key := wsName + "-users"
	if cached, ok := userDirCache[key]; ok {
		return cached, nil
	}
	var cached []slack.User
	if loadCache(key, &cached) {
		userDirCache[key] = cached
		return cached, nil
	}
	return fetchUsersLive(client, wsName)
}

// fetchUsersLive fetches the user directory from Slack (bypassing the cache) and
// refreshes the cache. 1000 (Slack's documented max) rather than 200 per page
// is a deliberate choice, not the default — measured live against a workspace
// with tens of thousands of users, requesting 200/page took over 6 minutes
// (dozens of requests), 1000/page took about 1 minute for the identical
// result. The bottleneck here is per-request overhead (rate-limit backoff,
// network latency), not data volume, so fewer/larger pages wins decisively.
func fetchUsersLive(client *slack.Client, wsName string) ([]slack.User, error) {
	users, err := client.GetUsers(slack.GetUsersOptionLimit(1000))
	if err != nil {
		return nil, err
	}
	saveCache(wsName+"-users", users)
	userDirCache[wsName+"-users"] = users
	return users, nil
}
