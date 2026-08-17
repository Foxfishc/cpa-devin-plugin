package main

import (
	"encoding/json"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	// defaultBaseURL is the Devin Connect RPC endpoint used by the desktop app.
	defaultBaseURL = "https://server.codeium.com"
	// defaultLoginURL is the browser page that mints a short-lived auth token.
	defaultLoginURL = "https://windsurf.com/show-auth-token"
	// defaultClientName is the client identity reported to Devin.
	defaultClientName = "chisel"
	// defaultClientVersion is the client version reported to Devin.
	defaultClientVersion = "3000.2.17"
	// sessionTokenPrefix marks a Devin session token.
	sessionTokenPrefix = "devin-session-token$"
	// loginFlowTTL bounds how long a started login flow stays pollable.
	loginFlowTTL = 10 * time.Minute
)

var currentConfig atomic.Value

// pluginConfig holds the plugin-owned configuration block.
type pluginConfig struct {
	Enabled           bool   `yaml:"enabled"`
	BaseURL           string `yaml:"base_url"`
	LoginURL          string `yaml:"login_url"`
	ClientName        string `yaml:"client_name"`
	ClientVersion     string `yaml:"client_version"`
	OS                string `yaml:"os"`
	Locale            string `yaml:"locale"`
	ImportFromDesktop bool   `yaml:"import_from_desktop"`
	DesktopStateDB    string `yaml:"desktop_state_db"`
	MaxTokens         int32  `yaml:"max_tokens"`
}

// defaultPluginConfig returns the configuration used when the host supplies none.
func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:           true,
		BaseURL:           defaultBaseURL,
		LoginURL:          defaultLoginURL,
		ClientName:        defaultClientName,
		ClientVersion:     defaultClientVersion,
		OS:                defaultDevinOS(),
		Locale:            "en",
		ImportFromDesktop: true,
		MaxTokens:         128000,
	}
}

// defaultDevinOS maps the running platform onto the Devin metadata os value.
func defaultDevinOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "win"
	default:
		return "linux"
	}
}

// configure decodes the host lifecycle payload into the active configuration.
func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	currentConfig.Store(normalizeConfig(cfg))
	return nil
}

// normalizeConfig trims user input and restores defaults for empty fields.
func normalizeConfig(cfg pluginConfig) pluginConfig {
	defaults := defaultPluginConfig()
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaults.BaseURL
	}
	cfg.LoginURL = strings.TrimSpace(cfg.LoginURL)
	if cfg.LoginURL == "" {
		cfg.LoginURL = defaults.LoginURL
	}
	cfg.ClientName = strings.TrimSpace(cfg.ClientName)
	if cfg.ClientName == "" {
		cfg.ClientName = defaults.ClientName
	}
	cfg.ClientVersion = strings.TrimSpace(cfg.ClientVersion)
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = defaults.ClientVersion
	}
	cfg.OS = strings.TrimSpace(cfg.OS)
	if cfg.OS == "" {
		cfg.OS = defaults.OS
	}
	cfg.Locale = strings.TrimSpace(cfg.Locale)
	if cfg.Locale == "" {
		cfg.Locale = defaults.Locale
	}
	cfg.DesktopStateDB = strings.TrimSpace(cfg.DesktopStateDB)
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = defaults.MaxTokens
	}
	return cfg
}

// loadedConfig returns the active configuration.
func loadedConfig() pluginConfig {
	if cfg, ok := currentConfig.Load().(pluginConfig); ok {
		return cfg
	}
	return defaultPluginConfig()
}

// configFields describes plugin configuration for management clients.
func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "When false, the Devin provider declines login, model, and execution requests."},
		{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Devin Connect RPC base URL. Defaults to https://server.codeium.com."},
		{Name: "login_url", Type: pluginapi.ConfigFieldTypeString, Description: "Browser page that displays the short-lived Devin auth token."},
		{Name: "client_name", Type: pluginapi.ConfigFieldTypeString, Description: "Client identity reported to Devin. Defaults to chisel."},
		{Name: "client_version", Type: pluginapi.ConfigFieldTypeString, Description: "Client version reported to Devin. Defaults to 3000.2.17."},
		{Name: "os", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"mac", "win", "linux"}, Description: "Operating system reported to Devin. Defaults to the host platform."},
		{Name: "locale", Type: pluginapi.ConfigFieldTypeString, Description: "Locale reported to Devin. Defaults to en."},
		{Name: "import_from_desktop", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Allow importing a session token from a locally installed Devin Desktop."},
		{Name: "desktop_state_db", Type: pluginapi.ConfigFieldTypeString, Description: "Explicit path to the Devin Desktop state.vscdb. Empty means auto-detect."},
		{Name: "max_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "Upstream completion max_tokens sent to Devin. Defaults to 128000."},
	}
}
