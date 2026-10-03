package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func chatContentToResponses(content any, role string) any {
	blocks, ok := content.([]any)
	if !ok {
		return content
	}
	out := make([]any, 0, len(blocks))
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		switch block["type"] {
		case "text":
			kind := "input_text"
			if role == "assistant" {
				kind = "output_text"
			}
			out = append(out, map[string]any{"type": kind, "text": block["text"]})
		case "image_url":
			image, _ := block["image_url"].(map[string]any)
			if image == nil {
				image = map[string]any{"url": block["image_url"]}
			}
			part := map[string]any{"type": "input_image", "image_url": image["url"]}
			if detail := image["detail"]; detail != nil {
				part["detail"] = detail
			}
			out = append(out, part)
		}
	}
	return out
}

func chatToResponsesRequest(params map[string]any, stream bool) map[string]any {
	body := map[string]any{"model": params["model"], "stream": stream, "store": false}
	for _, key := range []string{"temperature", "top_p", "metadata", "parallel_tool_calls", "user", "prompt_cache_key", "safety_identifier"} {
		if value, ok := params[key]; ok {
			body[key] = value
		}
	}
	if maxTokens := params["max_completion_tokens"]; maxTokens != nil {
		body["max_output_tokens"] = maxTokens
	} else if maxTokens := params["max_tokens"]; maxTokens != nil {
		body["max_output_tokens"] = maxTokens
	}
	if effort, ok := params["reasoning_effort"].(string); ok && effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	if format, _ := params["response_format"].(map[string]any); format != nil {
		switch format["type"] {
		case "json_schema":
			if schema, _ := format["json_schema"].(map[string]any); schema != nil {
				flat := map[string]any{"type": "json_schema", "name": schema["name"], "schema": schema["schema"]}
				if strict, ok := schema["strict"].(bool); ok {
					flat["strict"] = strict
				}
				body["text"] = map[string]any{"format": flat}
			}
		case "json_object":
			body["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		}
	}
	if tools, _ := params["tools"].([]any); len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			if tool["type"] != "function" || fn == nil {
				continue
			}
			flat := map[string]any{"type": "function"}
			for _, key := range []string{"name", "description", "parameters", "strict"} {
				if value, ok := fn[key]; ok {
					flat[key] = value
				}
			}
			converted = append(converted, flat)
		}
		if len(converted) > 0 {
			body["tools"] = converted
		}
	}
	if choice, ok := params["tool_choice"].(map[string]any); ok {
		if fn, _ := choice["function"].(map[string]any); fn != nil {
			body["tool_choice"] = map[string]any{"type": "function", "name": fn["name"]}
		}
	} else if choice := params["tool_choice"]; choice != nil {
		body["tool_choice"] = choice
	}
	inputs := []any{}
	messages, _ := params["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		switch role {
		case "tool":
			inputs = append(inputs, map[string]any{"type": "function_call_output", "call_id": message["tool_call_id"], "output": message["content"]})
		case "system", "developer", "user", "assistant":
			if content := message["content"]; content != nil && (content != "" || role != "assistant") {
				inputs = append(inputs, map[string]any{"role": role, "content": chatContentToResponses(content, role)})
			}
			if calls, _ := message["tool_calls"].([]any); role == "assistant" {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					if fn != nil {
						inputs = append(inputs, map[string]any{"type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"]})
					}
				}
			}
		}
	}
	body["input"] = inputs
	return body
}

func responsesUsageToChat(value any) map[string]any {
	usage, _ := value.(map[string]any)
	if usage == nil {
		return nil
	}
	input, output := tokenCount(usage["input_tokens"]), tokenCount(usage["output_tokens"])
	result := map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
	if details, _ := usage["input_tokens_details"].(map[string]any); details != nil {
		result["prompt_tokens_details"] = map[string]any{"cached_tokens": tokenCount(details["cached_tokens"])}
	}
	if details, _ := usage["output_tokens_details"].(map[string]any); details != nil {
		result["completion_tokens_details"] = map[string]any{"reasoning_tokens": tokenCount(details["reasoning_tokens"])}
	}
	return result
}

func responsesOutputToChat(response map[string]any) map[string]any {
	message := map[string]any{"role": "assistant", "content": ""}
	var text, reasoning strings.Builder
	toolCalls := []any{}
	outputs, _ := response["output"].([]any)
	for _, raw := range outputs {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "message":
			parts, _ := item["content"].([]any)
			for _, rawPart := range parts {
				part, _ := rawPart.(map[string]any)
				if part["type"] == "output_text" || part["type"] == "text" {
					value, _ := part["text"].(string)
					text.WriteString(value)
				}
			}
		case "reasoning":
			if value := responsesReasoningText(item); value != "" {
				if reasoning.Len() > 0 {
					reasoning.WriteByte('\n')
				}
				reasoning.WriteString(value)
			}
		case "function_call", "custom_tool_call":
			arguments, _ := item["arguments"].(string)
			if item["type"] == "custom_tool_call" {
				input, _ := item["input"].(string)
				encoded, _ := json.Marshal(map[string]any{"input": input})
				arguments = string(encoded)
			}
			toolCalls = append(toolCalls, map[string]any{"id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": arguments}})
		}
	}
	message["content"] = text.String()
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	if response["status"] == "incomplete" {
		finish = "length"
		if details, _ := response["incomplete_details"].(map[string]any); details != nil && details["reason"] == "content_filter" {
			finish = "content_filter"
		}
	}
	created := response["created_at"]
	if created == nil {
		created = time.Now().Unix()
	}
	return map[string]any{
		"id": response["id"], "object": "chat.completion", "created": created, "model": response["model"],
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   responsesUsageToChat(response["usage"]),
	}
}

func (responsesProviderAdapter) Chat(ctx context.Context, provider CustomProvider, params map[string]any, stream bool) (*http.Response, error) {
	upstreamStream := stream || provider.ForceStream
	encoded, err := json.Marshal(chatToResponsesRequest(requestParamsWithoutInternalMetadata(params), upstreamStream))
	if err != nil {
		return nil, fmt.Errorf("encode Responses request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.BaseURL+"/responses", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header = providerHeaders(provider, true)
	response, err := customProviderClient(provider).Do(request)
	if err != nil {
		return nil, fmt.Errorf("provider request: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		body := readAllLimited(response.Body, 64<<10)
		response.Body.Close()
		return nil, newUpstreamHTTPError(response.StatusCode, string(body))
	}
	var chat map[string]any
	if upstreamStream {
		response = responsesStreamToChatResponse(response)
		if stream {
			return response, nil
		}
		chat, err = aggregateChatCompletionStream(response.Body)
		response.Body.Close()
	} else {
		var object map[string]any
		err = json.NewDecoder(response.Body).Decode(&object)
		response.Body.Close()
		if err == nil && (object["status"] == "failed" || object["error"] != nil) {
			return nil, newUpstreamHTTPError(http.StatusBadGateway, fmt.Sprint(object["error"]))
		}
		if err == nil {
			chat = responsesOutputToChat(object)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("decode Responses response: %w", err)
	}
	result, err := json.Marshal(chat)
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(result))
	response.ContentLength = int64(len(result))
	response.Header.Set("Content-Type", "application/json")
	return response, nil
}

func responsesStreamToChatResponse(response *http.Response) *http.Response {
	original := response.Body
	reader, writer := io.Pipe()
	response.Body = reader
	response.ContentLength = -1
	response.Header = response.Header.Clone()
	response.Header.Set("Content-Type", "text/event-stream")
	go convertResponsesStreamToChat(original, writer)
	return response
}

func convertResponsesStreamToChat(source io.ReadCloser, destination *io.PipeWriter) {
	defer source.Close()
	defer destination.Close()
	id := "chatcmpl_" + secureRandomHex(12)
	model := ""
	created := time.Now().Unix()
	type streamItem struct {
		kind, name, callID         string
		arguments, text, reasoning strings.Builder
		toolIndex                  int
	}
	items := map[int]*streamItem{}
	nextToolIndex := 0
	hasToolCall := false
	finished := false
	emit := func(delta map[string]any, finish any, usage any) error {
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			chunk["usage"] = responsesUsageToChat(usage)
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(destination, "data: %s\n\n", encoded)
		return err
	}
	toolDelta := func(item *streamItem, arguments string, initial bool) map[string]any {
		call := map[string]any{"index": item.toolIndex, "function": map[string]any{"arguments": arguments}}
		if initial {
			call["id"] = item.callID
			call["type"] = "function"
			call["function"].(map[string]any)["name"] = item.name
		}
		return map[string]any{"tool_calls": []any{call}}
	}
	reader := bufio.NewReader(source)
	for {
		line, readErr := reader.ReadString('\n')
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
			if payload != "" && payload != "[DONE]" {
				var event map[string]any
				if err := json.Unmarshal([]byte(payload), &event); err == nil {
					typeName, _ := event["type"].(string)
					index := int(tokenCount(event["output_index"]))
					item := items[index]
					switch typeName {
					case "response.created":
						response, _ := event["response"].(map[string]any)
						if value, _ := response["id"].(string); value != "" {
							id = value
						}
						model, _ = response["model"].(string)
						if value := tokenCount(response["created_at"]); value > 0 {
							created = value
						}
						if err := emit(map[string]any{"role": "assistant"}, nil, nil); err != nil {
							return
						}
					case "response.output_item.added":
						output, _ := event["item"].(map[string]any)
						item = &streamItem{toolIndex: -1}
						item.kind, _ = output["type"].(string)
						items[index] = item
						if item.kind == "function_call" || item.kind == "custom_tool_call" {
							item.name, _ = output["name"].(string)
							item.callID, _ = output["call_id"].(string)
							item.toolIndex = nextToolIndex
							nextToolIndex++
							hasToolCall = true
							if err := emit(toolDelta(item, "", true), nil, nil); err != nil {
								return
							}
						}
					case "response.output_text.delta":
						if item == nil {
							item = &streamItem{kind: "message"}
							items[index] = item
						}
						value, _ := event["delta"].(string)
						item.text.WriteString(value)
						if value != "" {
							if err := emit(map[string]any{"content": value}, nil, nil); err != nil {
								return
							}
						}
					case "response.reasoning_summary_text.delta":
						if item == nil {
							item = &streamItem{kind: "reasoning"}
							items[index] = item
						}
						value, _ := event["delta"].(string)
						item.reasoning.WriteString(value)
						if value != "" {
							if err := emit(map[string]any{"reasoning_content": value}, nil, nil); err != nil {
								return
							}
						}
					case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
						if item != nil {
							value, _ := event["delta"].(string)
							item.arguments.WriteString(value)
							if value != "" && item.kind == "function_call" {
								if err := emit(toolDelta(item, value, false), nil, nil); err != nil {
									return
								}
							}
						}
					case "response.output_text.done", "response.reasoning_summary_text.done":
						if item != nil {
							value, _ := event["text"].(string)
							if typeName == "response.output_text.done" {
								if tail := strings.TrimPrefix(value, item.text.String()); tail != "" {
									if err := emit(map[string]any{"content": tail}, nil, nil); err != nil {
										return
									}
								}
								item.text.Reset()
								item.text.WriteString(value)
							} else {
								if tail := strings.TrimPrefix(value, item.reasoning.String()); tail != "" {
									if err := emit(map[string]any{"reasoning_content": tail}, nil, nil); err != nil {
										return
									}
								}
								item.reasoning.Reset()
								item.reasoning.WriteString(value)
							}
						}
					case "response.output_item.done":
						output, _ := event["item"].(map[string]any)
						if item != nil && item.kind == "message" {
							parts, _ := output["content"].([]any)
							var complete strings.Builder
							for _, rawPart := range parts {
								part, _ := rawPart.(map[string]any)
								if part["type"] == "output_text" {
									value, _ := part["text"].(string)
									complete.WriteString(value)
								}
							}
							if tail := strings.TrimPrefix(complete.String(), item.text.String()); tail != "" {
								if err := emit(map[string]any{"content": tail}, nil, nil); err != nil {
									return
								}
							}
						} else if item != nil && item.kind == "reasoning" {
							if tail := strings.TrimPrefix(responsesReasoningText(output), item.reasoning.String()); tail != "" {
								if err := emit(map[string]any{"reasoning_content": tail}, nil, nil); err != nil {
									return
								}
							}
						} else if item != nil && (item.kind == "function_call" || item.kind == "custom_tool_call") {
							value, _ := output["arguments"].(string)
							if item.kind == "custom_tool_call" {
								input, _ := output["input"].(string)
								encoded, _ := json.Marshal(map[string]any{"input": input})
								value = string(encoded)
							}
							seen := item.arguments.String()
							if item.kind == "custom_tool_call" {
								seen = ""
							}
							if tail := strings.TrimPrefix(value, seen); tail != "" {
								if err := emit(toolDelta(item, tail, false), nil, nil); err != nil {
									return
								}
							}
						}
					case "response.completed", "response.incomplete":
						response, _ := event["response"].(map[string]any)
						if model == "" {
							model, _ = response["model"].(string)
						}
						finish := "stop"
						if hasToolCall {
							finish = "tool_calls"
						}
						if typeName == "response.incomplete" {
							finish = "length"
						}
						if err := emit(map[string]any{}, finish, response["usage"]); err != nil {
							return
						}
						_, _ = io.WriteString(destination, "data: [DONE]\n\n")
						finished = true
					case "error", "response.failed":
						failure := event["error"]
						if failure == nil {
							response, _ := event["response"].(map[string]any)
							failure = response["error"]
						}
						encoded, _ := json.Marshal(map[string]any{"error": failure})
						_, _ = fmt.Fprintf(destination, "data: %s\n\n", encoded)
						return
					}
				}
			}
		}
		if finished {
			return
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				_ = destination.CloseWithError(readErr)
			}
			return
		}
	}
}
