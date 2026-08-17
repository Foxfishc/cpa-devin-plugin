package main

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	devinproto "cpa-devin-plugin/internal/devinproto"
)

// Devin chat enum aliases.
const (
	chatSourceUser   = devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER
	chatSourceSystem = devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM
	chatSourceTool   = devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL
)

// historyImagePlaceholder replaces images dropped from earlier turns.
const historyImagePlaceholder = "[Image omitted from history]"

// openAIRequest is the subset of the Chat Completions request the plugin maps.
type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Tools       []openAITool    `json:"tools"`
	Stream      bool            `json:"stream"`
	MaxTokens   *uint64         `json:"max_tokens"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
}

// openAIMessage is one Chat Completions message.
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`
}

// openAIToolCall is one assistant tool call.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAITool is one tool declaration.
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// contentPart is one structured content block.
type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// messageContent holds the flattened text and images of one message.
type messageContent struct {
	text   string
	images []*devinproto.ExaCodeiumCommonPb_ImageData
}

// buildChatRequest converts a Chat Completions payload into a Devin chat request.
func buildChatRequest(client *devinClient, cfg pluginConfig, model string, payload []byte) (*devinproto.GetChatMessageRequest, error) {
	var parsed openAIRequest
	if errUnmarshal := json.Unmarshal(payload, &parsed); errUnmarshal != nil {
		return nil, errors.New("devin: invalid chat completions payload")
	}
	modelUID := strings.TrimSpace(model)
	if modelUID == "" {
		modelUID = strings.TrimSpace(parsed.Model)
	}
	if modelUID == "" {
		return nil, errors.New("devin: request has no model")
	}

	systemPrompt, prompts := buildPrompts(parsed.Messages)
	systemPrompt = withToolDescriptions(systemPrompt, parsed.Tools)

	request := &devinproto.GetChatMessageRequest{
		Metadata:            client.metadata(),
		Prompt:              proto.String(systemPrompt),
		ChatModelUid:        proto.String(modelUID),
		RequestType:         devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
		Configuration:       buildConfiguration(cfg, parsed),
		ChatMessagePrompts:  prompts,
		Tools:               buildToolDefinitions(parsed.Tools),
		TrajectoryReference: buildTrajectoryReference(),
		CascadeId:           proto.String(uuid.NewString()),
		ExecutionId:         proto.String(uuid.NewString()),
		PlannerMode:         devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT.Enum(),
	}
	return request, nil
}

// buildConfiguration maps sampling parameters onto the Devin completion configuration.
func buildConfiguration(cfg pluginConfig, parsed openAIRequest) *devinproto.ExaCodeiumCommonPb_CompletionConfiguration {
	maxTokens := uint64(cfg.MaxTokens)
	if parsed.MaxTokens != nil && *parsed.MaxTokens > 0 {
		maxTokens = *parsed.MaxTokens
	}
	temperature := 1.0
	if parsed.Temperature != nil {
		temperature = *parsed.Temperature
	}
	topP := 0.95
	if parsed.TopP != nil && *parsed.TopP > 0 {
		topP = *parsed.TopP
	}
	return &devinproto.ExaCodeiumCommonPb_CompletionConfiguration{
		NumCompletions: proto.Uint64(1),
		MaxTokens:      proto.Uint64(maxTokens),
		MaxNewlines:    proto.Uint64(400),
		Temperature:    proto.Float64(temperature),
		TopK:           proto.Uint64(40),
		TopP:           proto.Float64(topP),
	}
}

// buildTrajectoryReference builds the Cascade trajectory reference for one turn.
func buildTrajectoryReference() *devinproto.ExaCortexPb_CortexTrajectoryReference {
	return &devinproto.ExaCortexPb_CortexTrajectoryReference{
		TrajectoryId:   proto.String(uuid.NewString()),
		TrajectoryType: devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE.Enum(),
		StepType:       devinproto.ExaCortexPb_CortexStepType_ExaCortexPb_CortexStepType_CORTEX_STEP_TYPE_USER_INPUT.Enum(),
	}
}

// buildPrompts splits messages into the system prompt and the Devin prompt list.
func buildPrompts(messages []openAIMessage) (string, []*devinproto.ExaChatPb_ChatMessagePrompt) {
	systemParts := make([]string, 0, 2)
	prompts := make([]*devinproto.ExaChatPb_ChatMessagePrompt, 0, len(messages))
	currentRoundStart := currentRoundIndex(messages)

	for index, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		content := decodeContent(message.Content)
		if role == "system" || role == "developer" {
			if text := strings.TrimSpace(content.text); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		keepImages := index >= currentRoundStart
		text := content.text
		if !keepImages && len(content.images) > 0 {
			text = appendPlaceholders(text, len(content.images))
		}
		prompt := &devinproto.ExaChatPb_ChatMessagePrompt{
			MessageId: proto.String(uuid.NewString()),
			Prompt:    proto.String(text),
		}
		if keepImages && len(content.images) > 0 {
			prompt.Images = content.images
		}
		switch role {
		case "assistant":
			prompt.Source = chatSourceSystem.Enum()
			prompt.ToolCalls = buildToolCalls(message.ToolCalls)
		case "tool", "function":
			prompt.Source = chatSourceTool.Enum()
			if id := strings.TrimSpace(message.ToolCallID); id != "" {
				prompt.ToolCallId = proto.String(id)
			}
		default:
			prompt.Source = chatSourceUser.Enum()
		}
		prompts = append(prompts, prompt)
	}
	return strings.Join(systemParts, "\n\n"), prompts
}

// currentRoundIndex returns the index where the latest turn begins.
// Images are only forwarded for messages at or after this index.
func currentRoundIndex(messages []openAIMessage) int {
	for index := len(messages) - 1; index >= 0; index-- {
		if strings.EqualFold(strings.TrimSpace(messages[index].Role), "assistant") {
			return index + 1
		}
	}
	return 0
}

// appendPlaceholders records dropped historical images in the message text.
func appendPlaceholders(text string, count int) string {
	placeholders := make([]string, 0, count)
	for i := 0; i < count; i++ {
		placeholders = append(placeholders, historyImagePlaceholder)
	}
	joined := strings.Join(placeholders, "\n")
	if strings.TrimSpace(text) == "" {
		return joined
	}
	return text + "\n" + joined
}

// decodeContent flattens a message content value into text and images.
func decodeContent(raw json.RawMessage) messageContent {
	result := messageContent{}
	if len(raw) == 0 {
		return result
	}
	var text string
	if errString := json.Unmarshal(raw, &text); errString == nil {
		result.text = text
		return result
	}
	var parts []contentPart
	if errParts := json.Unmarshal(raw, &parts); errParts != nil {
		return result
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part.Type)) {
		case "image_url":
			if image := decodeImage(part.ImageURL.URL); image != nil {
				result.images = append(result.images, image)
			}
		default:
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
	}
	result.text = strings.Join(texts, "\n")
	return result
}

// decodeImage converts a data URL or bare base64 payload into Devin image data.
func decodeImage(url string) *devinproto.ExaCodeiumCommonPb_ImageData {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil
	}
	mimeType := "image/png"
	data := url
	if strings.HasPrefix(url, "data:") {
		remainder := strings.TrimPrefix(url, "data:")
		comma := strings.Index(remainder, ",")
		if comma < 0 {
			return nil
		}
		header := remainder[:comma]
		data = remainder[comma+1:]
		if semicolon := strings.Index(header, ";"); semicolon >= 0 {
			header = header[:semicolon]
		}
		if strings.TrimSpace(header) != "" {
			mimeType = strings.TrimSpace(header)
		}
	} else if strings.HasPrefix(strings.ToLower(url), "http") {
		// Devin only accepts inline image bytes.
		return nil
	}
	if strings.TrimSpace(data) == "" {
		return nil
	}
	return &devinproto.ExaCodeiumCommonPb_ImageData{
		Base64Data: proto.String(data),
		MimeType:   proto.String(mimeType),
	}
}

// buildToolCalls converts assistant tool calls into Devin tool calls.
func buildToolCalls(calls []openAIToolCall) []*devinproto.ExaCodeiumCommonPb_ChatToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]*devinproto.ExaCodeiumCommonPb_ChatToolCall, 0, len(calls))
	for _, call := range calls {
		name := strings.TrimSpace(call.Function.Name)
		if name == "" {
			continue
		}
		arguments := strings.TrimSpace(call.Function.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		out = append(out, &devinproto.ExaCodeiumCommonPb_ChatToolCall{
			Id:            proto.String(strings.TrimSpace(call.ID)),
			Name:          proto.String(name),
			ArgumentsJson: proto.String(arguments),
		})
	}
	return out
}

// buildToolDefinitions converts tool declarations into Devin tool definitions.
func buildToolDefinitions(tools []openAITool) []*devinproto.ExaChatPb_ChatToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]*devinproto.ExaChatPb_ChatToolDefinition, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		schema := strings.TrimSpace(string(tool.Function.Parameters))
		if schema == "" {
			schema = "{}"
		}
		out = append(out, &devinproto.ExaChatPb_ChatToolDefinition{
			Name: proto.String(name),
			// Devin carries the schema here and the prose description in the
			// system prompt, matching the desktop client behaviour.
			Description:      proto.String(name),
			JsonSchemaString: proto.String(schema),
		})
	}
	return out
}

// withToolDescriptions appends tool prose descriptions to the system prompt.
func withToolDescriptions(systemPrompt string, tools []openAITool) string {
	var section strings.Builder
	for _, tool := range tools {
		description := strings.TrimSpace(tool.Function.Description)
		name := strings.TrimSpace(tool.Function.Name)
		if description == "" || name == "" {
			continue
		}
		if section.Len() == 0 {
			section.WriteString("# tools descriptions")
		}
		section.WriteString("\n<tool name=\"")
		section.WriteString(escapeXMLAttribute(name))
		section.WriteString("\">\n")
		section.WriteString(escapeXMLText(description))
		section.WriteString("\n</tool>")
	}
	if section.Len() == 0 {
		return systemPrompt
	}
	trimmed := strings.TrimRight(systemPrompt, "\r\n")
	if strings.TrimSpace(trimmed) == "" {
		return section.String()
	}
	return trimmed + "\n\n" + section.String()
}

// escapeXMLAttribute escapes a value used inside an XML attribute.
func escapeXMLAttribute(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

// escapeXMLText escapes a value used inside XML character data.
func escapeXMLText(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}
