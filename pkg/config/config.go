package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"agy-patcher/internal/hostenv"
)

// DNSProvider stores one user-configured or catalog DNS resolver entry in config.json.
type DNSProvider struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Target      string   `json:"target"`
	FallbackIPs []string `json:"fallback_ips,omitempty"`
	Enabled     bool     `json:"enabled"`
	Source      string   `json:"source,omitempty"`
}

// Config persists user preferences (language, custom paths, replacements, deselected targets, DNS) across sessions.
type Config struct {
	Lang             string            `json:"lang,omitempty"`
	ManualPaths      []string          `json:"manual_paths,omitempty"`
	ReplacedPaths    map[string]string `json:"replaced_paths,omitempty"`
	Deselected       []string          `json:"deselected,omitempty"`
	DNSMode          string            `json:"dns_mode,omitempty"`
	DNSCatalogURL    string            `json:"dns_catalog_url,omitempty"`
	DNSCustomized    bool              `json:"dns_customized,omitempty"`
	DNSProviders     []DNSProvider     `json:"dns_providers,omitempty"`
	UserDNSProviders []DNSProvider     `json:"user_dns_providers,omitempty"`
	HiddenDNSKeys    []string          `json:"hidden_dns_keys,omitempty"`
}

// FilePath returns the OS-appropriate config file location.
func FilePath() (string, error) {
	dir, err := hostenv.AppConfigDir(hostenv.AppDirName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads saved settings from FilePath, returning an empty Config if absent or unreadable.
func Load() Config {
	p, err := FilePath()
	if err != nil {
		return Config{}
	}
	cfg, err := LoadFrom(p)
	if err != nil {
		return Config{}
	}
	return cfg
}

// LoadFrom reads settings from the specified file path.
func LoadFrom(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, err
	}
	cfg.Lang = normalizeStoredLang(cfg.Lang)
	return cfg, nil
}

// Save writes cfg atomically to FilePath.
func Save(cfg Config) error {
	p, err := FilePath()
	if err != nil {
		return err
	}
	return SaveTo(p, cfg)
}

// SaveTo writes cfg atomically to the specified file path.
func SaveTo(path string, cfg Config) error {
	cfg.Lang = normalizeStoredLang(cfg.Lang)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return hostenv.WriteFileAtomic(path, data, 0644, nil)
}

// Remove deletes the config file and removes its directory if empty.
func Remove() error {
	p, err := FilePath()
	if err != nil {
		return nil
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(filepath.Dir(p))
	return nil
}

func normalizeStoredLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "ru":
		return "ru"
	case "en":
		return "en"
	default:
		return ""
	}
}
