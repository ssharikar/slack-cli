package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Workspace holds credentials for a single Slack workspace.
type Workspace struct {
	Token     string `json:"token"`
	Cookie    string `json:"cookie"`
	UserAgent string `json:"user_agent,omitempty"`
}

// JiraConfig holds credentials for the optional Jira integration, used to
// resolve a person's name to their Jira username when the Slack directory
// doesn't have an exact match for it.
type JiraConfig struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
}

// Config holds all workspace profiles and the default selection.
type Config struct {
	Default    string                `json:"default"`
	Workspaces map[string]*Workspace `json:"workspaces"`
	Jira       *JiraConfig           `json:"jira,omitempty"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".slack-cli")
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
}

// CacheDir returns the directory used for cached workspace directories
// (users/channels), creating it if necessary.
func CacheDir() (string, error) {
	dir := filepath.Join(configDir(), "cache")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// ResolveName returns the effective workspace name for the given flag value,
// falling back to the configured default. It's used to key per-workspace caches.
func ResolveName(workspace string) (string, error) {
	cfg, err := LoadFull()
	if err != nil {
		return "", err
	}
	name := workspace
	if name == "" {
		name = cfg.Default
	}
	if name == "" {
		return "", fmt.Errorf("no default workspace set — run 'slack-cli auth login' or use -w <workspace>")
	}
	if _, ok := cfg.Workspaces[name]; !ok {
		return "", fmt.Errorf("workspace '%s' not found in config", name)
	}
	return name, nil
}

// LoadFull loads the entire config file.
func LoadFull() (*Config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil, fmt.Errorf("no config found — run 'slack-cli auth login' first")
	}

	// Try loading legacy single-workspace format first
	var legacy struct {
		Token  string `json:"token"`
		Cookie string `json:"cookie"`
	}
	if err := json.Unmarshal(data, &legacy); err == nil && legacy.Token != "" && legacy.Cookie != "" {
		// Check if it's really the old format (has token at top level, no workspaces key)
		var raw map[string]json.RawMessage
		json.Unmarshal(data, &raw)
		if _, hasWorkspaces := raw["workspaces"]; !hasWorkspaces {
			cfg := Config{
				Default:    "default",
				Workspaces: map[string]*Workspace{"default": {Token: legacy.Token, Cookie: legacy.Cookie}},
			}
			// Auto-migrate to new format
			saveFull(&cfg)
			return &cfg, nil
		}
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid config file: %w", err)
	}

	if cfg.Workspaces == nil {
		cfg.Workspaces = make(map[string]*Workspace)
	}
	return &cfg, nil
}

// Load returns the workspace credentials for the given name, or the default.
func Load(workspace string) (*Workspace, error) {
	cfg, err := LoadFull()
	if err != nil {
		return nil, err
	}

	name := workspace
	if name == "" {
		name = cfg.Default
	}
	if name == "" {
		return nil, fmt.Errorf("no default workspace set — run 'slack-cli auth login' or use -w <workspace>")
	}

	ws, ok := cfg.Workspaces[name]
	if !ok {
		return nil, fmt.Errorf("workspace '%s' not found in config — run 'slack-cli auth login'", name)
	}
	return ws, nil
}

// SaveWorkspace saves credentials for a named workspace and sets it as default if it's the first.
func SaveWorkspace(name string, ws *Workspace) error {
	cfg, err := LoadFull()
	if err != nil {
		cfg = &Config{Workspaces: make(map[string]*Workspace)}
	}

	cfg.Workspaces[name] = ws

	// Set as default if it's the first workspace or no default is set
	if cfg.Default == "" || len(cfg.Workspaces) == 1 {
		cfg.Default = name
	}

	return saveFull(cfg)
}

// SetDefault changes the default workspace.
func SetDefault(name string) error {
	cfg, err := LoadFull()
	if err != nil {
		return err
	}

	if _, ok := cfg.Workspaces[name]; !ok {
		return fmt.Errorf("workspace '%s' not found in config", name)
	}

	cfg.Default = name
	return saveFull(cfg)
}

// LoadJira returns the configured Jira credentials, or an error if the
// integration hasn't been set up yet.
func LoadJira() (*JiraConfig, error) {
	cfg, err := LoadFull()
	if err != nil {
		return nil, err
	}
	if cfg.Jira == nil {
		return nil, fmt.Errorf("no Jira connection configured — run 'slack-cli jira config'")
	}
	return cfg.Jira, nil
}

// SaveJira saves Jira credentials, creating the config file if it doesn't
// exist yet (Jira can be set up before or after any Slack workspace).
func SaveJira(jira *JiraConfig) error {
	cfg, err := LoadFull()
	if err != nil {
		// Only start fresh when there is no config file at all. Any other
		// error (e.g. a file that fails to parse) must not be papered over,
		// or saving would overwrite every stored workspace credential.
		if _, statErr := os.Stat(configPath()); !os.IsNotExist(statErr) {
			return err
		}
		cfg = &Config{Workspaces: make(map[string]*Workspace)}
	}
	cfg.Jira = jira
	return saveFull(cfg)
}

// ListWorkspaces returns sorted workspace names and the default name.
func ListWorkspaces() ([]string, string, error) {
	cfg, err := LoadFull()
	if err != nil {
		return nil, "", err
	}

	names := make([]string, 0, len(cfg.Workspaces))
	for name := range cfg.Workspaces {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, cfg.Default, nil
}

func saveFull(cfg *Config) error {
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}
