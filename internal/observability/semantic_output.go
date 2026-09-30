package observability

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// outputBlock is one normalized piece of assistant output retained in the
// Langfuse generation Output: readable text or a structured tool call.
type outputBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
}

// assistantOutput is the normalized semantic shape written to
// langfuse.observation.output whenever the assistant output is more than one
// plain text segment, regardless of which provider protocol produced it.
type assistantOutput struct {
	Role    string        `json:"role"`
	Content []outputBlock `json:"content"`
}

// semanticLLMOutput decodes captured upstream response bytes into the semantic
// LLM output the provider produced: wire compression is undone, a JSON body is
// mapped to assistant text and tool calls, and an SSE stream is reassembled
// into the final assistant message. It reports ok=false for any payload it
// cannot confidently interpret so callers keep the legacy raw rendering
// (fail-open). This is a pure function over already-captured bytes and never
// touches the proxy data path.
func semanticLLMOutput(raw []byte) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	data, ok := decodeWireBody(raw)
	if !ok {
		return nil, false
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 {
		return nil, false
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return jsonLLMOutput(trimmed)
	}
	return aggregatedSSEOutput(trimmed)
}

// decodeWireBody undoes the wire compression executors explicitly negotiate
// via Accept-Encoding (so Go's transport leaves the body encoded for the
// capture mirror). Encodings without a reliable magic sequence (brotli, raw
// deflate) are returned as-is and fail open downstream.
func decodeWireBody(raw []byte) ([]byte, bool) {
	switch {
	case len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b:
		return decodeCompressed(raw, func(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) })
	case len(raw) >= 4 && raw[0] == 0x28 && raw[1] == 0xb5 && raw[2] == 0x2f && raw[3] == 0xfd:
		decoder, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, false
		}
		defer decoder.Close()
		decoded, err := io.ReadAll(decoder)
		if err != nil {
			return nil, false
		}
		return decoded, true
	case len(raw) >= 2 && raw[0] == 0x78:
		return decodeCompressed(raw, func(r io.Reader) (io.ReadCloser, error) { return zlib.NewReader(r) })
	default:
		return raw, true
	}
}

func decodeCompressed(raw []byte, newReader func(io.Reader) (io.ReadCloser, error)) ([]byte, bool) {
	reader, err := newReader(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// jsonLLMOutput maps a complete JSON response body from any supported provider
// protocol onto the normalized assistant output shape.
func jsonLLMOutput(data []byte) (any, bool) {
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, false
	}
	if _, isError := body["error"]; isError {
		// Error bodies are already readable text; keep the legacy rendering.
		return nil, false
	}
	if choices, ok := body["choices"].([]any); ok && len(choices) == 1 {
		return openAIMessageOutput(choices[0])
	}
	if candidates, ok := body["candidates"].([]any); ok && len(candidates) == 1 {
		return geminiPartsOutput(candidates[0])
	}
	if items, ok := body["output"].([]any); ok {
		return responsesItemsOutput(items)
	}
	if blocks, ok := body["content"].([]any); ok {
		return anthropicBlocksOutput(blocks)
	}
	return nil, false
}

func openAIMessageOutput(choice any) (any, bool) {
	obj, ok := choice.(map[string]any)
	if !ok {
		return nil, false
	}
	message, ok := obj["message"].(map[string]any)
	if !ok {
		return nil, false
	}
	var blocks []outputBlock
	if text := stringContent(message["content"]); text != "" {
		blocks = append(blocks, outputBlock{Type: "text", Text: text})
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, item := range calls {
			call, _ := item.(map[string]any)
			function, _ := call["function"].(map[string]any)
			id, _ := call["id"].(string)
			name, _ := function["name"].(string)
			raw, _ := function["arguments"].(string)
			blocks = append(blocks, toolCallBlock(id, name, raw))
		}
	}
	return finalizeAssistant(blocks)
}

// stringContent renders message content that is either a plain string or an
// array of typed parts into one text segment.
func stringContent(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case []any:
		var builder strings.Builder
		for _, part := range content {
			if piece, ok := part.(map[string]any); ok {
				if text, ok := piece["text"].(string); ok {
					builder.WriteString(text)
				}
			} else if text, ok := part.(string); ok {
				builder.WriteString(text)
			}
		}
		return builder.String()
	default:
		return ""
	}
}

// toolCallBlock normalizes a tool call into the shared block shape, keeping
// JSON arguments as structure and degrading to the raw string only when the
// arguments are not valid JSON.
func toolCallBlock(id, name, rawArguments string) outputBlock {
	var arguments any = rawArguments
	if rawArguments != "" && json.Valid([]byte(rawArguments)) {
		_ = json.Unmarshal([]byte(rawArguments), &arguments)
	}
	return outputBlock{Type: "tool_call", ID: id, Name: name, Arguments: arguments}
}

func anthropicBlocksOutput(blocks []any) (any, bool) {
	var normalized []outputBlock
	for _, item := range blocks {
		block, _ := item.(map[string]any)
		kind, _ := block["type"].(string)
		switch kind {
		case "text":
			text, _ := block["text"].(string)
			normalized = append(normalized, outputBlock{Type: "text", Text: text})
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			call := toolCallBlock(id, name, "")
			if input := block["input"]; input != nil {
				call.Arguments = input
			}
			normalized = append(normalized, call)
		}
	}
	return finalizeAssistant(normalized)
}

func geminiPartsOutput(candidate any) (any, bool) {
	obj, ok := candidate.(map[string]any)
	if !ok {
		return nil, false
	}
	content, _ := obj["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	var normalized []outputBlock
	for _, item := range parts {
		part, _ := item.(map[string]any)
		if thought, _ := part["thought"].(bool); thought {
			continue
		}
		if text, ok := part["text"].(string); ok {
			normalized = append(normalized, outputBlock{Type: "text", Text: text})
		}
		if call, ok := part["functionCall"].(map[string]any); ok {
			name, _ := call["name"].(string)
			callBlock := toolCallBlock("", name, "")
			if args := call["args"]; args != nil {
				callBlock.Arguments = args
			}
			normalized = append(normalized, callBlock)
		}
	}
	return finalizeAssistant(normalized)
}

func responsesItemsOutput(items []any) (any, bool) {
	var normalized []outputBlock
	for _, item := range items {
		entry, _ := item.(map[string]any)
		kind, _ := entry["type"].(string)
		switch kind {
		case "message":
			var text strings.Builder
			if parts, ok := entry["content"].([]any); ok {
				for _, part := range parts {
					piece, _ := part.(map[string]any)
					if pieceText, ok := piece["text"].(string); ok {
						text.WriteString(pieceText)
					}
				}
			}
			if text.Len() > 0 {
				normalized = append(normalized, outputBlock{Type: "text", Text: text.String()})
			}
		case "function_call":
			id, _ := entry["call_id"].(string)
			if id == "" {
				id, _ = entry["id"].(string)
			}
			name, _ := entry["name"].(string)
			raw, _ := entry["arguments"].(string)
			normalized = append(normalized, toolCallBlock(id, name, raw))
		}
	}
	return finalizeAssistant(normalized)
}

func finalizeAssistant(blocks []outputBlock) (any, bool) {
	if len(blocks) == 0 {
		return nil, false
	}
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return blocks[0].Text, true
	}
	return assistantOutput{Role: "assistant", Content: blocks}, true
}

// sseEvent is one parsed Server-Sent Event frame.
type sseEvent struct {
	name string
	data []byte
}

// parseSSE splits an SSE byte stream into events, joining multi-line data
// fields per the SSE specification.
func parseSSE(data []byte) []sseEvent {
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	var events []sseEvent
	for _, chunk := range bytes.Split(normalized, []byte("\n\n")) {
		var (
			name  string
			lines [][]byte
		)
		for _, line := range bytes.Split(chunk, []byte("\n")) {
			field, value, found := bytes.Cut(line, []byte{':'})
			if !found {
				continue
			}
			value = bytes.TrimPrefix(value, []byte{' '})
			switch string(field) {
			case "event":
				name = string(value)
			case "data":
				lines = append(lines, value)
			}
		}
		if len(lines) > 0 {
			events = append(events, sseEvent{name: name, data: bytes.Join(lines, []byte("\n"))})
		}
	}
	return events
}

// streamedBlock accumulates one output block (text or tool call) across the
// fragments a stream delivers for it.
type streamedBlock struct {
	blockType string
	id        string
	name      string
	raw       strings.Builder
}

// aggregatedSSEOutput reassembles streamed provider events into the final
// assistant message. The stream family is detected from each event payload, so
// any supported protocol works without a priori knowledge of the request.
func aggregatedSSEOutput(data []byte) (any, bool) {
	events := parseSSE(data)
	if len(events) == 0 {
		return nil, false
	}

	var (
		text        strings.Builder
		calls       = map[int]*streamedBlock{}
		callOrder   []int
		anthropic   = map[int]*streamedBlock{}
		blockOrder  []int
		responsesID = map[string]*streamedBlock{}
		responses   []string
		completed   any
		sawPayload  bool
	)
	for _, event := range events {
		var payload map[string]any
		if err := json.Unmarshal(event.data, &payload); err != nil {
			continue // [DONE] markers, heartbeats, and keep-alives
		}
		sawPayload = true
		switch {
		case payloadType(payload) == "response.completed":
			if response, ok := payload["response"].(map[string]any); ok {
				completed, _ = response["output"].([]any)
			}
		case strings.HasPrefix(payloadType(payload), "response."):
			collectResponsesEvent(payload, responsesID, &responses, &text)
		case isAnthropicEvent(payload, event.name):
			collectAnthropicEvent(payload, anthropic, &blockOrder)
		case hasArray(payload, "choices"):
			collectChatDelta(payload, calls, &callOrder, &text)
		case hasArray(payload, "candidates"):
			collectGeminiChunk(payload, calls, &callOrder, &text)
		}
	}
	if !sawPayload {
		return nil, false
	}
	if completed != nil {
		items, _ := completed.([]any)
		return responsesItemsOutput(items)
	}

	var merged []outputBlock
	if text.Len() > 0 {
		merged = append(merged, outputBlock{Type: "text", Text: text.String()})
	}
	for _, call := range orderedBlocks(calls, callOrder) {
		merged = append(merged, finishStreamedBlock(call))
	}
	for _, call := range orderedBlocks(anthropic, blockOrder) {
		merged = append(merged, finishStreamedBlock(call))
	}
	for _, id := range responses {
		merged = append(merged, finishStreamedBlock(responsesID[id]))
	}
	return finalizeAssistant(merged)
}

func finishStreamedBlock(call *streamedBlock) outputBlock {
	if call == nil {
		return outputBlock{Type: "text"}
	}
	if call.blockType == "text" {
		return outputBlock{Type: "text", Text: call.raw.String()}
	}
	return toolCallBlock(call.id, call.name, call.raw.String())
}

func payloadType(payload map[string]any) string {
	kind, _ := payload["type"].(string)
	return kind
}

func hasArray(payload map[string]any, key string) bool {
	_, ok := payload[key].([]any)
	return ok
}

func isAnthropicEvent(payload map[string]any, eventName string) bool {
	switch payloadType(payload) {
	case "message_start", "content_block_start", "content_block_delta", "message_delta", "message_stop":
		return true
	}
	switch eventName {
	case "message_start", "content_block_start", "content_block_delta", "message_delta", "message_stop":
		return true
	}
	return false
}

// collectChatDelta merges one OpenAI-compatible chat chunk: text deltas are
// concatenated and tool-call fragments are merged by their stream index.
func collectChatDelta(payload map[string]any, calls map[int]*streamedBlock, order *[]int, text *strings.Builder) {
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	if piece, ok := delta["content"].(string); ok {
		text.WriteString(piece)
	}
	fragments, _ := delta["tool_calls"].([]any)
	for _, item := range fragments {
		fragment, _ := item.(map[string]any)
		index := 0
		if raw, ok := fragment["index"].(float64); ok {
			index = int(raw)
		}
		call := calls[index]
		if call == nil {
			call = &streamedBlock{blockType: "tool_call"}
			calls[index] = call
			*order = append(*order, index)
		}
		if id, ok := fragment["id"].(string); ok && id != "" {
			call.id = id
		}
		if function, ok := fragment["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && name != "" {
				call.name = name
			}
			if piece, ok := function["arguments"].(string); ok {
				call.raw.WriteString(piece)
			}
		}
	}
}

// collectAnthropicEvent merges one Anthropic streaming event: content blocks
// are tracked by index, with text deltas and tool input JSON accumulated into
// their own blocks.
func collectAnthropicEvent(payload map[string]any, blocks map[int]*streamedBlock, order *[]int) {
	index := 0
	if raw, ok := payload["index"].(float64); ok {
		index = int(raw)
	}
	switch payloadType(payload) {
	case "content_block_start":
		block, _ := payload["content_block"].(map[string]any)
		kind, _ := block["type"].(string)
		if kind != "text" && kind != "tool_use" {
			return
		}
		entry := &streamedBlock{blockType: kind}
		if kind == "text" {
			if text, ok := block["text"].(string); ok {
				entry.raw.WriteString(text)
			}
		} else {
			entry.id, _ = block["id"].(string)
			entry.name, _ = block["name"].(string)
		}
		blocks[index] = entry
		*order = append(*order, index)
	case "content_block_delta":
		entry := blocks[index]
		if entry == nil {
			return
		}
		delta, _ := payload["delta"].(map[string]any)
		switch deltaType, _ := delta["type"].(string); deltaType {
		case "text_delta":
			if piece, ok := delta["text"].(string); ok {
				entry.raw.WriteString(piece)
			}
		case "input_json_delta":
			if piece, ok := delta["partial_json"].(string); ok {
				entry.raw.WriteString(piece)
			}
		}
	}
}

// collectResponsesEvent merges OpenAI Responses streaming events into
// per-item accumulators keyed by item id.
func collectResponsesEvent(payload map[string]any, items map[string]*streamedBlock, order *[]string, text *strings.Builder) {
	itemID, _ := payload["item_id"].(string)
	switch payloadType(payload) {
	case "response.output_text.delta":
		if piece, ok := payload["delta"].(string); ok {
			text.WriteString(piece)
		}
	case "response.output_item.added":
		item, _ := payload["item"].(map[string]any)
		if itemType, _ := item["type"].(string); itemType == "function_call" {
			id, _ := item["id"].(string)
			entry := &streamedBlock{blockType: "tool_call"}
			entry.id, _ = item["call_id"].(string)
			entry.name, _ = item["name"].(string)
			items[id] = entry
			*order = append(*order, id)
		}
	case "response.function_call_arguments.delta":
		entry := items[itemID]
		if entry == nil {
			entry = &streamedBlock{blockType: "tool_call"}
			items[itemID] = entry
			*order = append(*order, itemID)
		}
		if piece, ok := payload["delta"].(string); ok {
			entry.raw.WriteString(piece)
		}
	}
}

// collectGeminiChunk merges one Gemini streaming chunk: text parts are
// concatenated and function calls are appended in arrival order.
func collectGeminiChunk(payload map[string]any, calls map[int]*streamedBlock, order *[]int, text *strings.Builder) {
	candidates, _ := payload["candidates"].([]any)
	if len(candidates) == 0 {
		return
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	for _, item := range parts {
		part, _ := item.(map[string]any)
		if thought, _ := part["thought"].(bool); thought {
			continue
		}
		if piece, ok := part["text"].(string); ok {
			text.WriteString(piece)
		}
		if call, ok := part["functionCall"].(map[string]any); ok {
			index := len(*order)
			entry := &streamedBlock{blockType: "tool_call"}
			entry.name, _ = call["name"].(string)
			if encoded, err := json.Marshal(call["args"]); err == nil {
				entry.raw.Write(encoded)
			}
			calls[index] = entry
			*order = append(*order, index)
		}
	}
}

func orderedBlocks(blocks map[int]*streamedBlock, order []int) []*streamedBlock {
	ordered := make([]*streamedBlock, 0, len(order))
	for _, index := range order {
		if block := blocks[index]; block != nil {
			ordered = append(ordered, block)
		}
	}
	return ordered
}
