package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// loginWithOneTimeToken exchanges a browser auth token for a Devin credential.
func loginWithOneTimeToken(ctx context.Context, cfg pluginConfig, oneTimeToken, source string) (devinStorage, error) {
	oneTimeToken = strings.TrimSpace(oneTimeToken)
	if oneTimeToken == "" {
		return devinStorage{}, errors.New("devin: authentication token is empty")
	}
	exchange := newDevinClient(cfg, "")
	sessionToken, apiKey, errExchange := exchange.exchangeOneTimeToken(ctx, oneTimeToken)
	if errExchange != nil {
		return devinStorage{}, errExchange
	}
	return buildStorage(ctx, cfg, sessionToken, apiKey, source)
}

// buildStorage validates a token and assembles the persisted credential.
func buildStorage(ctx context.Context, cfg pluginConfig, sessionToken, apiKey, source string) (devinStorage, error) {
	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" {
		return devinStorage{}, errors.New("devin: no session token available")
	}
	client := newDevinClient(cfg, sessionToken)
	email, errValidate := client.userLabel(ctx)
	if errValidate != nil {
		return devinStorage{}, fmt.Errorf("devin: credential validation failed: %w", errValidate)
	}
	return devinStorage{
		Type:         providerKey,
		SessionToken: sessionToken,
		APIKey:       strings.TrimSpace(apiKey),
		Email:        email,
		BaseURL:      cfg.BaseURL,
		Source:       source,
		LastRefresh:  time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// importFromDesktop recovers a session token from a local Devin Desktop install.
func importFromDesktop(ctx context.Context, cfg pluginConfig) (devinStorage, error) {
	if !cfg.ImportFromDesktop {
		return devinStorage{}, errors.New("devin: import_from_desktop is disabled")
	}
	paths := desktopStatePaths(cfg)
	if len(paths) == 0 {
		return devinStorage{}, errors.New("devin: no Devin Desktop state database found")
	}
	var lastErr error
	for _, path := range paths {
		tokens, errScan := scanSessionTokens(path)
		if errScan != nil {
			lastErr = errScan
			continue
		}
		for _, token := range tokens {
			storage, errBuild := buildStorage(ctx, cfg, token, "", sourceDesktop)
			if errBuild == nil {
				return storage, nil
			}
			lastErr = errBuild
		}
	}
	if lastErr != nil {
		return devinStorage{}, fmt.Errorf("devin: no usable session token in Devin Desktop state: %w", lastErr)
	}
	return devinStorage{}, errors.New("devin: no session token found in Devin Desktop state")
}

// desktopStatePaths lists candidate Devin Desktop state databases.
func desktopStatePaths(cfg pluginConfig) []string {
	if explicit := strings.TrimSpace(cfg.DesktopStateDB); explicit != "" {
		if _, errStat := os.Stat(explicit); errStat == nil {
			return []string{explicit}
		}
		return nil
	}
	// Devin Desktop is a VS Code derivative; the auth blob lives in globalStorage.
	appNames := []string{"Devin", "Windsurf", "Windsurf - Next"}
	roots := make([]string, 0, 2)
	switch runtime.GOOS {
	case "windows":
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			roots = append(roots, appData)
		}
	case "darwin":
		if home, errHome := os.UserHomeDir(); errHome == nil {
			roots = append(roots, filepath.Join(home, "Library", "Application Support"))
		}
	default:
		if configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); configHome != "" {
			roots = append(roots, configHome)
		}
		if home, errHome := os.UserHomeDir(); errHome == nil {
			roots = append(roots, filepath.Join(home, ".config"))
		}
	}
	paths := make([]string, 0, len(roots)*len(appNames))
	for _, root := range roots {
		for _, app := range appNames {
			candidate := filepath.Join(root, app, "User", "globalStorage", "state.vscdb")
			if _, errStat := os.Stat(candidate); errStat == nil {
				paths = append(paths, candidate)
			}
		}
	}
	return paths
}

// maxStateDBSize bounds how much of the state database is scanned.
const maxStateDBSize = 256 << 20

// scanSessionTokens extracts Devin session tokens from a state database.
//
// The file is a SQLite database, but the tokens are stored verbatim inside a JSON
// value and carry a distinctive prefix. Scanning the raw bytes read-only avoids a
// SQLite dependency; candidates are validated against the server before use, so a
// stale token recovered from a freed page is rejected rather than trusted.
func scanSessionTokens(path string) ([]string, error) {
	info, errStat := os.Stat(path)
	if errStat != nil {
		return nil, errStat
	}
	if info.Size() > maxStateDBSize {
		return nil, fmt.Errorf("devin: state database %s is too large", path)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, errRead
	}
	seen := make(map[string]struct{})
	tokens := make([]string, 0, 4)
	prefix := []byte(sessionTokenPrefix)
	for offset := 0; offset < len(data); {
		index := bytes.Index(data[offset:], prefix)
		if index < 0 {
			break
		}
		start := offset + index
		end := start + len(sessionTokenPrefix)
		for end < len(data) && isTokenByte(data[end]) {
			end++
		}
		token := string(data[start:end])
		if len(token) > len(sessionTokenPrefix) {
			if _, exists := seen[token]; !exists {
				seen[token] = struct{}{}
				tokens = append(tokens, token)
			}
		}
		offset = end
		if offset >= len(data) {
			break
		}
	}
	return tokens, nil
}

// isTokenByte reports whether b can appear inside a Devin session token.
func isTokenByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-' || b == '_' || b == '.' || b == '$':
		return true
	default:
		return false
	}
}
