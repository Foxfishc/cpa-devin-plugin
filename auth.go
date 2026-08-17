package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// credentialSource records how a credential was obtained.
const (
	sourceBrowser = "browser"
	sourceDesktop = "desktop-import"
	sourceManual  = "manual"
)

// devinStorage is the persisted Devin credential payload.
type devinStorage struct {
	Type         string `json:"type"`
	SessionToken string `json:"session_token"`
	APIKey       string `json:"api_key,omitempty"`
	Email        string `json:"email,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	Source       string `json:"source,omitempty"`
	LastRefresh  string `json:"last_refresh,omitempty"`
}

// activeToken returns the token used for upstream calls.
func (s devinStorage) activeToken() string {
	if token := strings.TrimSpace(s.SessionToken); token != "" {
		return token
	}
	return strings.TrimSpace(s.APIKey)
}

// pendingLogin tracks one in-flight browser login flow.
type pendingLogin struct {
	mu        sync.Mutex
	expiresAt time.Time
	token     string
	failure   string
}

var pendingLogins sync.Map

// handleAuthParse decides whether an auth file belongs to this provider.
func handleAuthParse(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	storage, errDecode := decodeStorage(req.RawJSON)
	if errDecode != nil || !strings.EqualFold(strings.TrimSpace(storage.Type), providerKey) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if storage.activeToken() == "" {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    authDataFromStorage(storage, req.FileName, req.RawJSON),
	})
}

// handleAuthLoginStart opens a browser login flow and returns its poll state.
func handleAuthLoginStart(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return nil, errors.New("devin: plugin is disabled")
	}
	state := providerKey + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	expiresAt := time.Now().Add(loginFlowTTL)
	pendingLogins.Store(state, &pendingLogin{expiresAt: expiresAt})
	prunePendingLogins()
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerKey,
		URL:       cfg.LoginURL,
		State:     state,
		ExpiresAt: expiresAt.UTC(),
		Metadata: map[string]any{
			"state":        state,
			"submit_path":  "/v0/management/" + submitTokenPath,
			"instructions": "Sign in on the opened page, copy the displayed authentication token, then POST it to submit_path as {\"state\":\"...\",\"token\":\"...\"}.",
		},
	})
}

// handleAuthLoginPoll reports login progress and returns the credential on success.
func handleAuthLoginPoll(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		if metaState, ok := req.Metadata["state"].(string); ok {
			state = strings.TrimSpace(metaState)
		}
	}
	entryRaw, ok := pendingLogins.Load(state)
	if !ok {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "login flow is unknown or expired",
		})
	}
	entry, _ := entryRaw.(*pendingLogin)
	if entry == nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login flow is invalid"})
	}

	entry.mu.Lock()
	failure := entry.failure
	token := entry.token
	expired := time.Now().After(entry.expiresAt)
	entry.mu.Unlock()

	switch {
	case failure != "":
		pendingLogins.Delete(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: failure})
	case expired:
		pendingLogins.Delete(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login flow expired"})
	case token == "":
		// The host OAuth callback flow writes the code to a file under the auth
		// directory. When the user submits the token through the management UI's
		// callback URL field, pick it up here so the polling flow completes
		// without requiring a separate submit-token API call.
		if cbToken := readOAuthCallbackToken(req.Host.AuthDir, state); cbToken != "" {
			token = cbToken
		}
		if token == "" {
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for the authentication token"})
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	storage, errLogin := loginWithOneTimeToken(ctx, loadedConfig(), token, sourceBrowser)
	pendingLogins.Delete(state)
	if errLogin != nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errLogin.Error()})
	}
	authData, errBuild := authDataFromNewStorage(storage)
	if errBuild != nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errBuild.Error()})
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: authData})
}

// readOAuthCallbackToken reads the code field from the host-written OAuth
// callback file (.oauth-<provider>-<state>.oauth) when it exists.
func readOAuthCallbackToken(authDir, state string) string {
	authDir = strings.TrimSpace(authDir)
	state = strings.TrimSpace(state)
	if authDir == "" || state == "" {
		return ""
	}
	fileName := fmt.Sprintf(".oauth-%s-%s.oauth", providerKey, state)
	data, errRead := os.ReadFile(filepath.Join(authDir, fileName))
	if errRead != nil {
		return ""
	}
	var payload struct {
		Code  string `json:"code"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return ""
	}
	if strings.TrimSpace(payload.Error) != "" {
		return ""
	}
	return strings.TrimSpace(payload.Code)
}

// handleAuthRefresh mints a fresh session token for a stored credential.
func handleAuthRefresh(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	storage, errDecode := decodeStorage(req.StorageJSON)
	if errDecode != nil {
		return nil, fmt.Errorf("devin: decode stored credential: %w", errDecode)
	}
	if storage.activeToken() == "" {
		return nil, errors.New("devin: stored credential has no token")
	}
	cfg := loadedConfig()
	if strings.TrimSpace(storage.BaseURL) != "" {
		cfg.BaseURL = strings.TrimRight(strings.TrimSpace(storage.BaseURL), "/")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := newDevinClient(cfg, storage.activeToken())
	refreshed, errRefresh := client.refreshSessionToken(ctx)
	if errRefresh != nil {
		// Session tokens have no fixed lifetime; keep the existing credential and
		// retry later instead of invalidating a possibly working token.
		hostLog("warn", "devin: session token refresh failed", map[string]any{"error": errRefresh.Error(), "auth_id": req.AuthID})
		return okEnvelope(pluginapi.AuthRefreshResponse{
			Auth:             authDataFromStorage(storage, req.AuthID, req.StorageJSON),
			NextRefreshAfter: time.Now().Add(15 * time.Minute).UTC(),
		})
	}
	storage.SessionToken = refreshed
	storage.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	rawStorage, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             authDataFromStorage(storage, req.AuthID, rawStorage),
		NextRefreshAfter: time.Now().Add(50 * time.Minute).UTC(),
	})
}

// decodeStorage parses a stored Devin credential payload.
func decodeStorage(raw []byte) (devinStorage, error) {
	var storage devinStorage
	if len(raw) == 0 {
		return storage, errors.New("empty credential payload")
	}
	if errUnmarshal := json.Unmarshal(raw, &storage); errUnmarshal != nil {
		return storage, errUnmarshal
	}
	return storage, nil
}

// authDataFromStorage builds the host auth record for an existing credential.
func authDataFromStorage(storage devinStorage, fileName string, rawStorage []byte) pluginapi.AuthData {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		fileName = credentialFileName(storage.Email)
	}
	label := strings.TrimSpace(storage.Email)
	if label == "" {
		label = "Devin Desktop"
	}
	metadata := map[string]any{
		"type":   providerKey,
		"email":  storage.Email,
		"source": storage.Source,
	}
	if storage.LastRefresh != "" {
		metadata["last_refresh"] = storage.LastRefresh
	}
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          fileName,
		FileName:    fileName,
		Label:       label,
		StorageJSON: rawStorage,
		Metadata:    metadata,
	}
}

// authDataFromNewStorage serializes a freshly created credential for the host.
func authDataFromNewStorage(storage devinStorage) (pluginapi.AuthData, error) {
	rawStorage, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		return pluginapi.AuthData{}, errMarshal
	}
	return authDataFromStorage(storage, credentialFileName(storage.Email), rawStorage), nil
}

// credentialFileName derives a stable auth file name for a Devin account.
func credentialFileName(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return fmt.Sprintf("%s-%d.json", providerKey, time.Now().UnixMilli())
	}
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, email)
	return fmt.Sprintf("%s-%s.json", providerKey, sanitized)
}

// prunePendingLogins drops expired login flows.
func prunePendingLogins() {
	now := time.Now()
	pendingLogins.Range(func(key, value any) bool {
		entry, ok := value.(*pendingLogin)
		if !ok {
			pendingLogins.Delete(key)
			return true
		}
		entry.mu.Lock()
		expired := now.After(entry.expiresAt)
		entry.mu.Unlock()
		if expired {
			pendingLogins.Delete(key)
		}
		return true
	})
}
