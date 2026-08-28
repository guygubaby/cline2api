package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func validateResponsesCompatibility(body map[string]any) error {
	if model, _ := body["model"].(string); model == "" {
		return fmt.Errorf("model is required")
	}
	if _, ok := body["input"]; !ok {
		return fmt.Errorf("input is required")
	}
	if background, _ := body["background"].(bool); background {
		return fmt.Errorf("background responses are not supported by this chat-completions upstream")
	}
	if previous, _ := body["previous_response_id"].(string); previous != "" {
		return fmt.Errorf("previous_response_id is not supported; resend prior output items in input")
	}
	if body["conversation"] != nil {
		return fmt.Errorf("conversation state is not supported; use stateless input items")
	}
	if body["prompt"] != nil {
		return fmt.Errorf("stored prompt references are not supported; send instructions and input directly")
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, tool := range tools {
			toolMap, _ := tool.(map[string]any)
			if toolMap == nil || toolMap["type"] != "function" {
				return fmt.Errorf("only custom function tools can be mapped to the upstream chat API")
			}
		}
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		if choice["type"] != "function" {
			return fmt.Errorf("only function tool_choice objects can be mapped to the upstream chat API")
		}
	}
	if input, ok := body["input"].([]any); ok {
		for _, item := range input {
			itemMap, _ := item.(map[string]any)
			content, _ := itemMap["content"].([]any)
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				switch block["type"] {
				case "input_file":
					return fmt.Errorf("input_file items are not supported by the upstream chat API")
				case "input_image":
					if imageURL, _ := block["image_url"].(string); imageURL == "" {
						return fmt.Errorf("input_image requires image_url; file_id images are not supported")
					}
				}
			}
		}
	}
	if text, ok := body["text"].(map[string]any); ok {
		if format, _ := text["format"].(map[string]any); format != nil && format["type"] == "json_schema" {
			name, _ := format["name"].(string)
			if name == "" || format["schema"] == nil {
				return fmt.Errorf("text.format json_schema requires name and schema")
			}
		}
	}
	return nil
}

// ============================================================================
// OpenAI Responses API (/v1/responses) ↔ chat/completions 双向转换
// 所有上游生效：zen 免费模型与 Cline 账号池均可通过该端点访问（Cursor 等客户端直连）。
// ============================================================================

// responsesToChat 将 Responses 请求体转换为 chat.completions 请求体。
func responsesToChat(body map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := body["model"].(string); ok {
		out["model"] = m
	}
	if s, ok := body["stream"].(bool); ok {
		out["stream"] = s
	}
	if mt, ok := body["max_output_tokens"].(float64); ok {
		out["max_tokens"] = int(mt)
	}
	for _, k := range []string{"temperature", "top_p", "stop", "seed", "user", "metadata", "logit_bias"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	msgs := responsesInputToMessages(body["input"])
	if instr, ok := body["instructions"].(string); ok && instr != "" {
		msgs = append([]any{map[string]any{"role": "system", "content": instr}}, msgs...)
	}
	out["messages"] = msgs
	if tools, ok := body["tools"].([]any); ok {
		out["tools"] = responsesToolsToChat(tools)
	}
	if tc, ok := body["tool_choice"]; ok {
		out["tool_choice"] = responsesToolChoiceToChat(tc)
	}
	if parallel, ok := body["parallel_tool_calls"].(bool); ok {
		out["parallel_tool_calls"] = parallel
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, _ := reasoning["effort"].(string); effort != "" {
			out["reasoning_effort"] = effort
		}
	}
	if text, ok := body["text"].(map[string]any); ok {
		if format, _ := text["format"].(map[string]any); format != nil {
			if responseFormat := responsesFormatToChat(format); responseFormat != nil {
				out["response_format"] = responseFormat
			}
		}
	}
	return out
}

// responsesInputToMessages 处理 string / item 数组两种 input 形态。
func responsesInputToMessages(input any) []any {
	var msgs []any
	switch v := input.(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		var pendingCalls []any
		flushCalls := func() {
			if len(pendingCalls) == 0 {
				return
			}
			msgs = append(msgs, map[string]any{
				"role":       "assistant",
				"content":    "",
				"tool_calls": pendingCalls,
			})
			pendingCalls = nil
		}
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "message", nil:
				flushCalls()
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				msgs = append(msgs, map[string]any{"role": role, "content": responsesContentToChat(m["content"])})
			case "function_call":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				name, _ := m["name"].(string)
				args := ""
				switch a := m["arguments"].(type) {
				case string:
					args = a
				case map[string]any:
					if b, err := json.Marshal(a); err == nil {
						args = string(b)
					}
				}
				pendingCalls = append(pendingCalls, map[string]any{
					"id":       callID,
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
			case "function_call_output":
				flushCalls()
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				output := ""
				switch o := m["output"].(type) {
				case string:
					output = o
				case []any:
					converted := responsesContentToChat(o)
					if text, ok := converted.(string); ok {
						output = text
					} else if b, err := json.Marshal(converted); err == nil {
						output = string(b)
					}
				case map[string]any:
					if b, err := json.Marshal(o); err == nil {
						output = string(b)
					}
				}
				msgs = append(msgs, map[string]any{"role": "tool", "content": output, "tool_call_id": callID})
			case "reasoning":
				// reasoning 输入项无法映射到 chat 输入，忽略
			}
		}
		flushCalls()
	}
	return msgs
}

func responsesContentToChat(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := []any{}
		textParts := []string{}
		allText := true
		for _, block := range v {
			if b, ok := block.(map[string]any); ok {
				switch b["type"] {
				case "input_text", "output_text", "text":
					if text, _ := b["text"].(string); text != "" {
						textParts = append(textParts, text)
						parts = append(parts, map[string]any{"type": "text", "text": text})
					}
				case "input_image":
					imageURL, _ := b["image_url"].(string)
					if imageURL == "" {
						continue
					}
					allText = false
					image := map[string]any{"url": imageURL}
					if detail, _ := b["detail"].(string); detail != "" {
						image["detail"] = detail
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": image})
				}
			}
		}
		if allText {
			return strings.Join(textParts, "\n")
		}
		return parts
	}
	return ""
}

func responsesToolChoiceToChat(choice any) any {
	mapped, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	if mapped["type"] == "function" {
		if name, _ := mapped["name"].(string); name != "" {
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			}
		}
	}
	return choice
}

func responsesFormatToChat(format map[string]any) map[string]any {
	switch format["type"] {
	case "json_schema":
		jsonSchema := map[string]any{
			"name":   format["name"],
			"schema": format["schema"],
		}
		if strict, ok := format["strict"].(bool); ok {
			jsonSchema["strict"] = strict
		}
		return map[string]any{
			"type":        "json_schema",
			"json_schema": jsonSchema,
		}
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "text":
		return map[string]any{"type": "text"}
	}
	return nil
}

// responsesToolsToChat 扁平 function 工具 → OpenAI 嵌套格式。
func responsesToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok || tm["type"] != "function" {
			continue
		}
		fn := map[string]any{}
		if n, ok := tm["name"].(string); ok {
			fn["name"] = n
		}
		if d, ok := tm["description"].(string); ok {
			fn["description"] = d
		}
		if p, ok := tm["parameters"].(map[string]any); ok {
			fn["parameters"] = p
		}
		if strict, ok := tm["strict"].(bool); ok {
			fn["strict"] = strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// ============ 非流式响应转换 ============

func newResponseID(prefix string) string {
	return prefix + fmt.Sprintf("%x", time.Now().UnixNano())
}

// chatToResponses 将 chat.completions 响应转换为 Responses 响应。
func chatToResponses(chat map[string]any) map[string]any {
	model, _ := chat["model"].(string)
	resp := newResponsesObject(newResponseID("resp_"), model, "completed")
	var outputs []any
	var outputText strings.Builder

	choices, _ := chat["choices"].([]any)
	if len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			msg, _ := ch["message"].(map[string]any)
			if msg == nil {
				msg, _ = ch["delta"].(map[string]any)
			}
			var text string
			if msg != nil {
				text, _ = msg["content"].(string)
			}
			toolCalls, _ := msg["tool_calls"].([]any)
			if text != "" {
				outputText.WriteString(text)
			}
			if text != "" || len(toolCalls) == 0 {
				content := []any{}
				if text != "" {
					content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
				}
				outputs = append(outputs, map[string]any{
					"type":    "message",
					"id":      newResponseID("msg_"),
					"status":  "completed",
					"role":    "assistant",
					"content": content,
				})
			}
			for _, call := range toolCalls {
				callMap, ok := call.(map[string]any)
				if !ok {
					continue
				}
				function, _ := callMap["function"].(map[string]any)
				callID, _ := callMap["id"].(string)
				if callID == "" {
					callID = newResponseID("call_")
				}
				name, arguments := "", ""
				if function != nil {
					name, _ = function["name"].(string)
					arguments, _ = function["arguments"].(string)
				}
				outputs = append(outputs, map[string]any{
					"type":      "function_call",
					"id":        newResponseID("fc_"),
					"call_id":   callID,
					"name":      name,
					"arguments": arguments,
					"status":    "completed",
				})
			}
			switch ch["finish_reason"] {
			case "length":
				resp["status"] = "incomplete"
				resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
				delete(resp, "completed_at")
			case "content_filter":
				resp["status"] = "incomplete"
				resp["incomplete_details"] = map[string]any{"reason": "content_filter"}
				delete(resp, "completed_at")
			}
		}
	}
	resp["output"] = outputs
	resp["output_text"] = outputText.String()
	if usage := chatUsageToResponses(chat["usage"]); usage != nil {
		resp["usage"] = usage
	}
	return resp
}

func chatToResponsesWithRequest(chat, request map[string]any) map[string]any {
	response := chatToResponses(chat)
	applyResponsesRequestFields(response, request)
	return response
}

func applyResponsesRequestFields(response, request map[string]any) {
	if request == nil {
		return
	}
	for _, key := range []string{
		"instructions", "max_output_tokens", "metadata", "parallel_tool_calls",
		"reasoning", "temperature", "text", "tool_choice", "tools", "top_p", "truncation",
	} {
		if value, ok := request[key]; ok {
			response[key] = value
		}
	}
}

func chatUsageToResponses(value any) map[string]any {
	usage, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	inputTokens := any(0)
	if value := usage["prompt_tokens"]; value != nil {
		inputTokens = value
	} else if value := usage["input_tokens"]; value != nil {
		inputTokens = value
	}
	outputTokens := any(0)
	if value := usage["completion_tokens"]; value != nil {
		outputTokens = value
	} else if value := usage["output_tokens"]; value != nil {
		outputTokens = value
	}
	totalTokens := any(0)
	if value := usage["total_tokens"]; value != nil {
		totalTokens = value
	}
	cachedTokens := any(0)
	cacheWriteTokens := any(0)
	if details, _ := usage["prompt_tokens_details"].(map[string]any); details != nil {
		if value := details["cached_tokens"]; value != nil {
			cachedTokens = value
		}
		if value := details["cache_write_tokens"]; value != nil {
			cacheWriteTokens = value
		}
	}
	reasoningTokens := any(0)
	if details, _ := usage["completion_tokens_details"].(map[string]any); details != nil {
		if value := details["reasoning_tokens"]; value != nil {
			reasoningTokens = value
		}
	} else if value := usage["reasoning_tokens"]; value != nil {
		reasoningTokens = value
	}
	return map[string]any{
		"input_tokens": inputTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens":      cachedTokens,
			"cache_write_tokens": cacheWriteTokens,
		},
		"output_tokens": outputTokens,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": reasoningTokens,
		},
		"total_tokens": totalTokens,
	}
}

func newResponsesObject(id, model, status string) map[string]any {
	response := map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           time.Now().Unix(),
		"status":               status,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                model,
		"output":               []any{},
		"output_text":          "",
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"store":                false,
		"temperature":          nil,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                nil,
		"truncation":           "disabled",
		"usage":                nil,
		"metadata":             map[string]any{},
	}
	if status == "completed" {
		response["completed_at"] = time.Now().Unix()
	}
	return response
}

// ============ 流式响应转换（chat SSE → Responses SSE） ============

type responsesSSEWriter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	respID   string
	model    string
	sequence int
}

func newResponsesSSE(w http.ResponseWriter) *responsesSSEWriter {
	f, _ := w.(http.Flusher)
	return &responsesSSEWriter{
		w:       w,
		flusher: f,
		respID:  newResponseID("resp_"),
	}
}

func (s *responsesSSEWriter) event(event string, data any) {
	payload, ok := data.(map[string]any)
	if !ok {
		payload = map[string]any{"data": data}
	}
	payload["type"] = event
	payload["sequence_number"] = s.sequence
	s.sequence++
	b, _ := json.Marshal(payload)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, string(b))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

type responsesToolCallAccumulator struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
	added       bool
}

// chatStreamToResponses 逐行读取上游 chat SSE，转换为完整 Responses 事件生命周期。
// onUsage 在收到上游 usage 时回调（用于请求日志/账号统计）。
func chatStreamToResponses(w http.ResponseWriter, upstream *http.Response, reqLog *RequestLog, acc *Account, requestParams ...map[string]any) {
	s := newResponsesSSE(w)
	if reqLog != nil {
		s.model = reqLog.Model
	}
	initial := newResponsesObject(s.respID, s.model, "in_progress")
	if len(requestParams) > 0 {
		applyResponsesRequestFields(initial, requestParams[0])
	}
	delete(initial, "completed_at")
	s.event("response.created", map[string]any{"response": initial})
	s.event("response.in_progress", map[string]any{"response": initial})

	nextOutputIndex := 0
	textOutputIndex := -1
	textItemID := ""
	textEmitted := false
	var outText strings.Builder
	toolCalls := map[int]*responsesToolCallAccumulator{}
	toolOrder := []int{}
	var latestUsage tokenUsage
	finishReason := ""
	streamError := ""
	firstOutputAt := time.Time{}
	startedAt := time.Now()
	if reqLog != nil {
		startedAt = reqLog.StartedAt
	}

	ensureTextItem := func() {
		if textEmitted {
			return
		}
		textEmitted = true
		textOutputIndex = nextOutputIndex
		nextOutputIndex++
		textItemID = newResponseID("msg_")
		s.event("response.output_item.added", map[string]any{
			"output_index": textOutputIndex,
			"item": map[string]any{
				"id": textItemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{},
			},
		})
		s.event("response.content_part.added", map[string]any{
			"item_id": textItemID, "output_index": textOutputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}

	reader := bufio.NewReader(upstream.Body)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(line[5:])
				if payload != "" && payload != "[DONE]" {
					var obj map[string]any
					if json.Unmarshal([]byte(payload), &obj) == nil {
						obj = unwrapDataEnvelope(obj)
						if upstreamError := obj["error"]; upstreamError != nil {
							encoded, _ := json.Marshal(upstreamError)
							streamError = string(encoded)
							break
						}
						if m, ok := obj["model"].(string); ok && m != "" {
							s.model = m
						}
						usage := parseTokenUsage(obj["usage"])
						if usage.Valid {
							latestUsage = mergeTokenUsage(latestUsage, usage)
						}
						choice, _ := getNested(obj, "choices", 0).(map[string]any)
						if reason, _ := choice["finish_reason"].(string); reason != "" {
							finishReason = reason
						}
						delta := getNested(obj, "choices", 0, "delta")
						if delta == nil {
							delta = getNested(obj, "choices", 0)
						}
						if d, ok := delta.(map[string]any); ok {
							if content, _ := d["content"].(string); content != "" {
								ensureTextItem()
								outText.WriteString(content)
								s.event("response.output_text.delta", map[string]any{
									"item_id": textItemID, "output_index": textOutputIndex, "content_index": 0, "delta": content,
								})
							}
							if calls, ok := d["tool_calls"].([]any); ok {
								for _, rawCall := range calls {
									callMap, _ := rawCall.(map[string]any)
									if callMap == nil {
										continue
									}
									upstreamIndex := 0
									if value, ok := callMap["index"].(float64); ok {
										upstreamIndex = int(value)
									}
									call := toolCalls[upstreamIndex]
									if call == nil {
										call = &responsesToolCallAccumulator{
											outputIndex: nextOutputIndex,
											itemID:      newResponseID("fc_"),
										}
										nextOutputIndex++
										toolCalls[upstreamIndex] = call
										toolOrder = append(toolOrder, upstreamIndex)
									}
									if id, _ := callMap["id"].(string); id != "" {
										call.callID = id
									}
									function, _ := callMap["function"].(map[string]any)
									if function != nil {
										if name, _ := function["name"].(string); name != "" {
											call.name = name
										}
									}
									if call.callID == "" {
										call.callID = newResponseID("call_")
									}
									if !call.added && call.name != "" {
										call.added = true
										s.event("response.output_item.added", map[string]any{
											"output_index": call.outputIndex,
											"item": map[string]any{
												"type": "function_call", "id": call.itemID, "call_id": call.callID,
												"name": call.name, "arguments": "", "status": "in_progress",
											},
										})
									}
									if function != nil {
										if arguments, _ := function["arguments"].(string); arguments != "" {
											call.arguments.WriteString(arguments)
											s.event("response.function_call_arguments.delta", map[string]any{
												"item_id": call.itemID, "output_index": call.outputIndex, "delta": arguments,
											})
										}
									}
								}
							}
							if firstOutputAt.IsZero() && hasFirstOutput(obj) {
								firstOutputAt = time.Now()
							}
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}

	outputs := make([]any, nextOutputIndex)
	if textEmitted {
		text := outText.String()
		s.event("response.output_text.done", map[string]any{
			"item_id": textItemID, "output_index": textOutputIndex, "content_index": 0, "text": text,
		})
		s.event("response.content_part.done", map[string]any{
			"item_id": textItemID, "output_index": textOutputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
		item := map[string]any{
			"id": textItemID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}
		s.event("response.output_item.done", map[string]any{
			"output_index": textOutputIndex,
			"item":         item,
		})
		outputs[textOutputIndex] = item
	}
	for _, upstreamIndex := range toolOrder {
		call := toolCalls[upstreamIndex]
		if !call.added && call.name != "" {
			call.added = true
			s.event("response.output_item.added", map[string]any{
				"output_index": call.outputIndex,
				"item": map[string]any{
					"type": "function_call", "id": call.itemID, "call_id": call.callID,
					"name": call.name, "arguments": "", "status": "in_progress",
				},
			})
		}
		arguments := call.arguments.String()
		s.event("response.function_call_arguments.done", map[string]any{
			"item_id": call.itemID, "output_index": call.outputIndex, "name": call.name, "arguments": arguments,
		})
		item := map[string]any{
			"type": "function_call", "id": call.itemID, "call_id": call.callID,
			"name": call.name, "arguments": arguments, "status": "completed",
		}
		s.event("response.output_item.done", map[string]any{
			"output_index": call.outputIndex,
			"item":         item,
		})
		outputs[call.outputIndex] = item
	}
	status := "completed"
	eventType := "response.completed"
	completed := true
	if streamError != "" {
		status = "failed"
		eventType = "response.failed"
		completed = false
	} else if finishReason == "length" || finishReason == "content_filter" {
		status = "incomplete"
		eventType = "response.incomplete"
	}
	response := newResponsesObject(s.respID, s.model, status)
	if len(requestParams) > 0 {
		applyResponsesRequestFields(response, requestParams[0])
	}
	response["output"] = outputs
	response["output_text"] = outText.String()
	response["usage"] = usageToResponses(latestUsage)
	if status == "failed" {
		response["error"] = map[string]any{"code": "upstream_error", "message": streamError}
		delete(response, "completed_at")
	} else if status == "incomplete" {
		reason := "max_output_tokens"
		if finishReason == "content_filter" {
			reason = "content_filter"
		}
		response["incomplete_details"] = map[string]any{"reason": reason}
		delete(response, "completed_at")
	}
	s.event(eventType, map[string]any{"response": response})

	if reqLog != nil {
		if acc != nil && latestUsage.Valid {
			recordTokenUsage(acc, reqLog.Model, latestUsage)
		}
		finalizeRequestLog(reqLog, latestUsage, firstOutputAt, startedAt, completed, streamError)
	}
}

// usageToResponses 把聚合的 tokenUsage 转成 Responses usage 结构。
func usageToResponses(u tokenUsage) map[string]any {
	cacheRead := u.CacheRead
	if cacheRead == 0 && u.CacheWrite == 0 && u.Cached > 0 {
		cacheRead = u.Cached
	}
	total := u.Total
	if total == 0 {
		total = u.Prompt + u.Completion
	}
	return map[string]any{
		"input_tokens": u.Prompt,
		"input_tokens_details": map[string]any{
			"cached_tokens":      cacheRead,
			"cache_write_tokens": u.CacheWrite,
		},
		"output_tokens": u.Completion,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": u.Reasoning,
		},
		"total_tokens": total,
	}
}

// ============ /v1/responses 入口 ============

func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var params map[string]any
	if err := json.Unmarshal(body, &params); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := validateResponsesCompatibility(params); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model, _ := params["model"].(string)
	isStream, _ := params["stream"].(bool)
	log.Printf("  responses: model=%s stream=%v", model, isStream)

	reqLog := RequestLog{StartedAt: time.Now(), Protocol: "responses", Model: model, Stream: isStream}

	chat := responsesToChat(params)
	chatModel, _ := chat["model"].(string)
	route := resolveModelRoute(chatModel)

	switch route.Route {
	case modelRouteReject:
		finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, "paid zen model rejected")
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q is a paid opencode model; only free models are proxied", chatModel))
		return
	case modelRouteZenUnavailable:
		msg := fmt.Sprintf("opencode zen is temporarily unavailable and model %q has no compatible cline fallback", chatModel)
		finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, msg)
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable_error", msg)
		return
	case modelRouteZen:
		reqLog.Upstream = upstreamOpenCode
		zm, _ := resolveZenInfo(chatModel)
		out := maybeCompact(chat, zm, requestSessionID(chat, r.Header))
		if out.changed {
			log.Printf("  responses %s", out.note)
		}
		upResp, err := callZenAPI(chat, isStream)
		if err != nil {
			log.Printf("  responses api error: %v", err)
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, err.Error())
			writeOpenAIError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		defer upResp.Body.Close()

		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusOK)
			chatStreamToResponses(w, upResp, &reqLog, nil, params)
			return
		}
		var raw map[string]any
		if err := json.NewDecoder(upResp.Body).Decode(&raw); err != nil {
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, "decode response: "+err.Error())
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "decode response: "+err.Error())
			return
		}
		out2 := normalizeOpenAIResponse(unwrapDataEnvelope(raw))
		usage := parseTokenUsage(out2["usage"])
		finalizeRequestLog(&reqLog, usage, time.Time{}, reqLog.StartedAt, true, "")
		writeJSON(w, http.StatusOK, chatToResponsesWithRequest(out2, params))

	default: // cline
		if route.Model != "" && route.Model != chatModel {
			chat["model"] = route.Model
		}
		reqLog.Upstream = upstreamCline
		if !isStream {
			out, acc, err := callClineNonStream(chat)
			if err != nil {
				log.Printf("  responses api error: %v", err)
				finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, err.Error())
				writeOpenAIError(w, http.StatusBadGateway, "api_error", err.Error())
				return
			}
			if acc != nil {
				reqLog.AccountID = acc.AccountID
				reqLog.AccountEmail = acc.Email
			}
			usage := parseTokenUsage(out["usage"])
			if acc != nil {
				recordTokenUsage(acc, reqLog.Model, usage)
			}
			finalizeRequestLog(&reqLog, usage, time.Time{}, reqLog.StartedAt, true, "")
			writeJSON(w, http.StatusOK, chatToResponsesWithRequest(out, params))
			return
		}

		upResp, acc, err := callClineAPI(chat, true)
		if err != nil {
			log.Printf("  responses api error: %v", err)
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, err.Error())
			writeOpenAIError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		defer upResp.Body.Close()
		if acc != nil {
			reqLog.AccountID = acc.AccountID
			reqLog.AccountEmail = acc.Email
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		chatStreamToResponses(w, upResp, &reqLog, acc, params)
		return
	}
}

// unwrapDataEnvelope 剥掉部分上游返回的 {data:{...}} 信封。
func unwrapDataEnvelope(obj map[string]any) map[string]any {
	if data, ok := obj["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			if _, hasChoices := d["choices"]; hasChoices {
				return d
			}
			if _, hasID := d["id"]; hasID {
				return d
			}
		}
	}
	return obj
}
