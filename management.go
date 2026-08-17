package main

import (
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
	State string `json:"state"`
	Token string `json:"token"`
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
	if state == "" || token == "" {
		return okEnvelope(managementJSON(http.StatusBadRequest, map[string]string{"error": "state and token are required"}))
	}
	entryRaw, ok := pendingLogins.Load(state)
	if !ok {
		return okEnvelope(managementJSON(http.StatusNotFound, map[string]string{"error": "unknown or expired login state"}))
	}
	entry, _ := entryRaw.(*pendingLogin)
	if entry == nil {
		return okEnvelope(managementJSON(http.StatusNotFound, map[string]string{"error": "invalid login state"}))
	}
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
	return okEnvelope(managementJSON(http.StatusOK, map[string]string{"status": "ok"}))
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
