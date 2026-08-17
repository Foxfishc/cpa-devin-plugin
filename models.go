package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	devinproto "cpa-devin-plugin/internal/devinproto"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// modelCacheTTL bounds how long a discovered model catalog is reused.
const modelCacheTTL = 5 * time.Minute

type modelCacheEntry struct {
	models    []pluginapi.ModelInfo
	fetchedAt time.Time
}

var (
	modelCacheMu sync.Mutex
	modelCache   = map[string]modelCacheEntry{}
)

// handleModelStatic reports models that do not require a credential.
// Devin models are always credential-bound, so the static list stays empty.
func handleModelStatic(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
}

// handleModelForAuth discovers the model catalog available to one credential.
func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	storage, errDecode := decodeStorage(req.StorageJSON)
	if errDecode != nil || storage.activeToken() == "" {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	if strings.TrimSpace(storage.BaseURL) != "" {
		cfg.BaseURL = strings.TrimRight(strings.TrimSpace(storage.BaseURL), "/")
	}

	cacheKey := req.AuthID
	if cached, ok := cachedModels(cacheKey); ok {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: cached})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	models, errFetch := fetchModels(ctx, cfg, storage.activeToken())
	if errFetch != nil {
		hostLog("warn", "devin: model discovery failed", map[string]any{"error": errFetch.Error(), "auth_id": req.AuthID})
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	storeModels(cacheKey, models)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

// fetchModels requests the Cascade model catalog for a credential.
func fetchModels(ctx context.Context, cfg pluginConfig, token string) ([]pluginapi.ModelInfo, error) {
	client := newDevinClient(cfg, token)
	req := connect.NewRequest(&devinproto.GetCascadeModelConfigsRequest{Metadata: client.metadata()})
	client.applyBasicAuth(req.Header())
	resp, errCall := client.apiServer.GetCascadeModelConfigs(ctx, req)
	if errCall != nil {
		return nil, normalizeConnectError(errCall)
	}
	configs := resp.Msg.GetClientModelConfigs()
	models := make([]pluginapi.ModelInfo, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))
	for _, item := range configs {
		if item == nil || item.GetDisabled() {
			continue
		}
		modelID := strings.TrimSpace(item.GetModelUid())
		if modelID == "" {
			continue
		}
		if _, exists := seen[modelID]; exists {
			continue
		}
		seen[modelID] = struct{}{}
		models = append(models, modelInfoFromConfig(modelID, item))
	}
	return models, nil
}

// modelInfoFromConfig converts a Devin model config into host model metadata.
func modelInfoFromConfig(modelID string, item *devinproto.ExaCodeiumCommonPb_ClientModelConfig) pluginapi.ModelInfo {
	displayName := strings.TrimSpace(item.GetLabel())
	if displayName == "" {
		displayName = modelID
	}
	inputModalities := []string{"text"}
	if item.GetSupportsImages() {
		inputModalities = append(inputModalities, "image")
	}
	info := pluginapi.ModelInfo{
		ID:                         modelID,
		Object:                     "model",
		OwnedBy:                    providerKey,
		Type:                       "chat",
		DisplayName:                displayName,
		Name:                       modelID,
		Description:                strings.TrimSpace(item.GetDescription()),
		SupportedGenerationMethods: []string{"chat"},
		SupportedInputModalities:   inputModalities,
		SupportedOutputModalities:  []string{"text"},
	}
	if maxTokens := int64(item.GetMaxTokens()); maxTokens > 0 {
		info.ContextLength = maxTokens
		info.InputTokenLimit = maxTokens
		info.MaxCompletionTokens = maxTokens
		info.OutputTokenLimit = maxTokens
	}
	return info
}

// cachedModels returns a cached catalog when it is still fresh.
func cachedModels(key string) ([]pluginapi.ModelInfo, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false
	}
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()
	entry, ok := modelCache[key]
	if !ok || time.Since(entry.fetchedAt) > modelCacheTTL {
		return nil, false
	}
	return entry.models, true
}

// storeModels caches a discovered catalog.
func storeModels(key string, models []pluginapi.ModelInfo) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	modelCacheMu.Lock()
	modelCache[key] = modelCacheEntry{models: models, fetchedAt: time.Now()}
	modelCacheMu.Unlock()
}
