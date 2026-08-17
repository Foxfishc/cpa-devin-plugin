package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	devinproto "cpa-devin-plugin/internal/devinproto"
)

// rpcExecutorRequest is the executor request plus host stream coordination fields.
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// chatAccumulator collects streaming Devin output into OpenAI-shaped results.
type chatAccumulator struct {
	responseID   string
	model        string
	text         strings.Builder
	reasoning    strings.Builder
	toolCalls    []*accumulatedToolCall
	toolIndex    map[string]int
	inputTokens  uint64
	outputTokens uint64
	finishReason string
}

// accumulatedToolCall collects one streamed tool call.
type accumulatedToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

// newChatAccumulator builds an accumulator for one request.
func newChatAccumulator(model string) *chatAccumulator {
	return &chatAccumulator{
		responseID: "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		model:      model,
		toolIndex:  map[string]int{},
	}
}

// handleExecute runs a non-streaming Devin chat request.
func handleExecute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	payload, errRun := runChatCompletion(context.Background(), req.ExecutorRequest)
	if errRun != nil {
		return errorEnvelope("executor_error", errRun.Error()), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// handleExecuteStream starts a streaming Devin chat request.
func handleExecuteStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	execReq := req.ExecutorRequest
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(streamID, fmt.Sprintf("devin: stream panic: %v", recovered))
			}
		}()
		if errRun := runChatStream(context.Background(), execReq, streamID); errRun != nil {
			closePluginStream(streamID, errRun.Error())
			return
		}
		closePluginStream(streamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// handleCountTokens reports a token count for the request.
// Devin exposes no token counting RPC, so the plugin returns zero.
func handleCountTokens(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0,"total_tokens":0}`)})
}

// prepareChat resolves the credential and builds the Devin request.
func prepareChat(req pluginapi.ExecutorRequest) (*devinClient, *devinproto.GetChatMessageRequest, string, error) {
	cfg := loadedConfig()
	if !cfg.Enabled {
		return nil, nil, "", errors.New("devin: plugin is disabled")
	}
	storage, errDecode := decodeStorage(req.StorageJSON)
	if errDecode != nil {
		return nil, nil, "", fmt.Errorf("devin: decode credential: %w", errDecode)
	}
	token := storage.activeToken()
	if token == "" {
		return nil, nil, "", errors.New("devin: credential has no token")
	}
	if base := strings.TrimSpace(storage.BaseURL); base != "" {
		cfg.BaseURL = strings.TrimRight(base, "/")
	}
	client := newDevinClient(cfg, token)
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	chatRequest, errBuild := buildChatRequest(client, cfg, req.Model, payload)
	if errBuild != nil {
		return nil, nil, "", errBuild
	}
	return client, chatRequest, chatRequest.GetChatModelUid(), nil
}

// openStream issues the Devin chat RPC and returns the server stream.
func openStream(ctx context.Context, client *devinClient, chatRequest *devinproto.GetChatMessageRequest) (*connect.ServerStreamForClient[devinproto.GetChatMessageResponse], error) {
	connectReq := connect.NewRequest(chatRequest)
	client.applyBasicAuth(connectReq.Header())
	stream, errCall := client.apiServer.GetChatMessage(ctx, connectReq)
	if errCall != nil {
		return nil, normalizeConnectError(errCall)
	}
	return stream, nil
}

// runChatCompletion executes a Devin chat request and returns a complete response.
func runChatCompletion(ctx context.Context, req pluginapi.ExecutorRequest) ([]byte, error) {
	client, chatRequest, model, errPrepare := prepareChat(req)
	if errPrepare != nil {
		return nil, errPrepare
	}
	stream, errOpen := openStream(ctx, client, chatRequest)
	if errOpen != nil {
		return nil, errOpen
	}
	defer func() {
		if errClose := stream.Close(); errClose != nil {
			hostLog("debug", "devin: close chat stream", map[string]any{"error": errClose.Error()})
		}
	}()

	acc := newChatAccumulator(model)
	for stream.Receive() {
		acc.consume(stream.Msg())
	}
	if errStream := stream.Err(); errStream != nil {
		return nil, normalizeConnectError(errStream)
	}
	return acc.completion()
}

// runChatStream executes a Devin chat request and emits OpenAI stream chunks.
func runChatStream(ctx context.Context, req pluginapi.ExecutorRequest, streamID string) error {
	client, chatRequest, model, errPrepare := prepareChat(req)
	if errPrepare != nil {
		return errPrepare
	}
	stream, errOpen := openStream(ctx, client, chatRequest)
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		if errClose := stream.Close(); errClose != nil {
			hostLog("debug", "devin: close chat stream", map[string]any{"error": errClose.Error()})
		}
	}()

	acc := newChatAccumulator(model)
	if errEmit := emitChunk(streamID, acc.chunk(map[string]any{"role": "assistant"}, nil, nil)); errEmit != nil {
		return errEmit
	}
	for stream.Receive() {
		message := stream.Msg()
		deltas := acc.consumeStreaming(message)
		for _, delta := range deltas {
			if errEmit := emitChunk(streamID, acc.chunk(delta, nil, nil)); errEmit != nil {
				return errEmit
			}
		}
	}
	if errStream := stream.Err(); errStream != nil {
		return normalizeConnectError(errStream)
	}
	finish := acc.resolvedFinishReason()
	if errEmit := emitChunk(streamID, acc.chunk(map[string]any{}, &finish, acc.usage())); errEmit != nil {
		return errEmit
	}
	return emitRaw(streamID, []byte("data: [DONE]\n\n"))
}

// consume folds one Devin frame into the accumulator.
func (a *chatAccumulator) consume(message *devinproto.GetChatMessageResponse) {
	a.consumeStreaming(message)
}

// consumeStreaming folds one Devin frame in and returns OpenAI delta objects.
func (a *chatAccumulator) consumeStreaming(message *devinproto.GetChatMessageResponse) []map[string]any {
	if message == nil {
		return nil
	}
	if actual := strings.TrimSpace(message.GetActualModelUid()); actual != "" {
		a.model = actual
	}
	if usage := message.GetUsage(); usage != nil {
		if input := usage.GetInputTokens(); input > 0 {
			a.inputTokens = input
		}
		if output := usage.GetOutputTokens(); output > 0 {
			a.outputTokens = output
		}
	}
	deltas := make([]map[string]any, 0, 2)
	if thinking := message.GetDeltaThinking(); thinking != "" {
		a.reasoning.WriteString(thinking)
		deltas = append(deltas, map[string]any{"reasoning_content": thinking})
	}
	if text := message.GetDeltaText(); text != "" {
		a.text.WriteString(text)
		deltas = append(deltas, map[string]any{"content": text})
	}
	if toolDeltas := a.consumeToolCalls(message.GetDeltaToolCalls()); len(toolDeltas) > 0 {
		deltas = append(deltas, map[string]any{"tool_calls": toolDeltas})
	}
	if reason := message.GetStopReason(); reason != devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_UNSPECIFIED {
		a.finishReason = mapStopReason(reason)
	}
	return deltas
}

// consumeToolCalls folds tool call deltas in and returns OpenAI tool call deltas.
func (a *chatAccumulator) consumeToolCalls(calls []*devinproto.ExaCodeiumCommonPb_ChatToolCall) []map[string]any {
	if len(calls) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		if call == nil {
			continue
		}
		id := strings.TrimSpace(call.GetId())
		name := strings.TrimSpace(call.GetName())
		arguments := call.GetArgumentsJson()
		key := id
		if key == "" {
			key = name
		}
		if key == "" {
			continue
		}
		index, exists := a.toolIndex[key]
		delta := map[string]any{}
		if !exists {
			index = len(a.toolCalls)
			a.toolIndex[key] = index
			a.toolCalls = append(a.toolCalls, &accumulatedToolCall{id: id, name: name})
			delta["id"] = id
			delta["type"] = "function"
			delta["function"] = map[string]any{"name": name, "arguments": arguments}
		} else {
			delta["function"] = map[string]any{"arguments": arguments}
		}
		entry := a.toolCalls[index]
		if entry.id == "" && id != "" {
			entry.id = id
		}
		if entry.name == "" && name != "" {
			entry.name = name
		}
		entry.arguments.WriteString(arguments)
		delta["index"] = index
		out = append(out, delta)
	}
	return out
}

// resolvedFinishReason returns the effective OpenAI finish reason.
func (a *chatAccumulator) resolvedFinishReason() string {
	if a.finishReason != "" {
		return a.finishReason
	}
	if len(a.toolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// usage returns the OpenAI usage block.
func (a *chatAccumulator) usage() map[string]any {
	return map[string]any{
		"prompt_tokens":     a.inputTokens,
		"completion_tokens": a.outputTokens,
		"total_tokens":      a.inputTokens + a.outputTokens,
	}
}

// chunk renders one Chat Completions stream chunk as an SSE frame.
func (a *chatAccumulator) chunk(delta map[string]any, finishReason *string, usage map[string]any) []byte {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	}
	body := map[string]any{
		"id":      a.responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   a.model,
		"choices": []any{choice},
	}
	if usage != nil {
		body["usage"] = usage
	}
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil
	}
	return append(append([]byte("data: "), raw...), '\n', '\n')
}

// completion renders the accumulated result as a Chat Completions response.
func (a *chatAccumulator) completion() ([]byte, error) {
	message := map[string]any{"role": "assistant"}
	if text := a.text.String(); text != "" {
		message["content"] = text
	} else {
		message["content"] = nil
	}
	if reasoning := a.reasoning.String(); reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(a.toolCalls) > 0 {
		calls := make([]any, 0, len(a.toolCalls))
		for _, call := range a.toolCalls {
			arguments := call.arguments.String()
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			calls = append(calls, map[string]any{
				"id":       call.id,
				"type":     "function",
				"function": map[string]any{"name": call.name, "arguments": arguments},
			})
		}
		message["tool_calls"] = calls
	}
	body := map[string]any{
		"id":      a.responseID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   a.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": a.resolvedFinishReason(),
		}},
		"usage": a.usage(),
	}
	return json.Marshal(body)
}

// mapStopReason maps a Devin stop reason onto an OpenAI finish reason.
func mapStopReason(reason devinproto.ExaCodeiumCommonPb_StopReason) string {
	switch reason {
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL:
		return "tool_calls"
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_MAX_TOKENS,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_INCOMPLETE,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_MAX_NEWLINES,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_PARTIAL:
		return "length"
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_CONTENT_FILTER:
		return "content_filter"
	default:
		return "stop"
	}
}

// emitChunk sends one rendered SSE frame to the host stream.
func emitChunk(streamID string, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	return emitRaw(streamID, payload)
}

// emitRaw sends raw bytes to the host stream.
func emitRaw(streamID string, payload []byte) error {
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	return errCall
}

// closePluginStream closes the host stream, optionally with an error.
func closePluginStream(streamID, message string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, map[string]any{
		"stream_id": streamID,
		"error":     strings.TrimSpace(message),
	})
}
