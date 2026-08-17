package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// submitTokenPath is the Management API path that accepts a pasted auth token.
const submitTokenPath = "devin/submit-token"

type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

type submitTokenRequest struct {
	State string `json:"state,omitempty"`
	Token string `json:"token"`
}

type submitTokenResponse struct {
	Status   string `json:"status"`
	Email    string `json:"email,omitempty"`
	Error    string `json:"error,omitempty"`
	FileName string `json:"file_name,omitempty"`
}

// handleManagementRegister declares the plugin-owned token submission route.
func handleManagementRegister() ([]byte, error) {
	return okEnvelope(managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{{
			Method: http.MethodPost,
			Path:   submitTokenPath,
		}},
	})
}

// handleManagementHandle processes plugin-owned Management API requests.
//
// When a token is submitted with a matching pending login state, the token is
// stored for that flow and the host polling mechanism picks it up.
// When a token is submitted without a state (or with an unknown state), the
// plugin performs the full exchange and credential save synchronously so the
// user only needs one call.
func handleManagementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if !strings.HasSuffix(strings.TrimRight(req.Path, "/"), submitTokenPath) {
		return okEnvelope(managementJSON(http.StatusNotFound, map[string]string{"error": "unknown plugin route"}))
	}
	var body submitTokenRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return okEnvelope(managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"}))
		}
	}
	state := strings.TrimSpace(body.State)
	if state == "" {
		state = strings.TrimSpace(req.Query.Get("state"))
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		token = strings.TrimSpace(req.Query.Get("token"))
	}
	if token == "" {
		return okEnvelope(managementJSON(http.StatusBadRequest, map[string]string{"error": "token is required"}))
	}

	// If a valid pending login state is provided, store the token for the
	// polling flow and return immediately.
	if state != "" {
		entryRaw, ok := pendingLogins.Load(state)
		if ok {
			entry, _ := entryRaw.(*pendingLogin)
			if entry != nil {
				entry.mu.Lock()
				expired := time.Now().After(entry.expiresAt)
				if !expired {
					entry.token = token
				}
				entry.mu.Unlock()
				if expired {
					pendingLogins.Delete(state)
					return okEnvelope(managementJSON(http.StatusGone, map[string]string{"error": "login flow expired"}))
				}
				return okEnvelope(managementJSON(http.StatusOK, submitTokenResponse{Status: "ok"}))
			}
		}
	}

	// No state or unknown state: do the full exchange + save synchronously.
	cfg := loadedConfig()
	if !cfg.Enabled {
		return okEnvelope(managementJSON(http.StatusForbidden, map[string]string{"error": "devin: plugin is disabled"}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	storage, errLogin := loginWithOneTimeToken(ctx, cfg, token, sourceBrowser)
	if errLogin != nil {
		return okEnvelope(managementJSON(http.StatusOK, submitTokenResponse{Status: "error", Error: errLogin.Error()}))
	}
	rawStorage, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		return okEnvelope(managementJSON(http.StatusInternalServerError, map[string]string{"error": "failed to encode credential"}))
	}
	fileName := credentialFileName(storage.Email)
	_, errSave := callHost("host.auth.save", map[string]any{
		"name": fileName,
		"json": json.RawMessage(rawStorage),
	})
	if errSave != nil {
		return okEnvelope(managementJSON(http.StatusOK, submitTokenResponse{Status: "error", Error: "failed to save credential: " + errSave.Error()}))
	}
	return okEnvelope(managementJSON(http.StatusOK, submitTokenResponse{
		Status:   "ok",
		Email:    storage.Email,
		FileName: fileName,
	}))
}

// managementJSON builds a JSON Management API response.
func managementJSON(status int, payload any) pluginapi.ManagementResponse {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		body = []byte(`{"error":"failed to encode response"}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}
}
