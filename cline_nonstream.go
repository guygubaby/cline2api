package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

var errEmptyResponseContent = errors.New("upstream stream completed without visible content or tool calls")

type chatToolCallAccumulator struct {
	id        string
	name      string
	arguments strings.Builder
}

type chatChoiceAccumulator struct {
	index           int
	role            string
	content         strings.Builder
	reasoning       strings.Builder
	refusal         strings.Builder
	finishReason    string
	logprobs        any
	toolCalls       map[int]*chatToolCallAccumulator
	toolCallIndexes []int
}

func shouldForceClineStream(params map[string]any, clientStream bool) bool {
	if clientStream {
		return false
	}
	model, _ := params["model"].(string)
	return model == virtualFreeModel || strings.HasPrefix(model, "deepseek/") || strings.HasPrefix(model, "z-ai/glm-5.3")
}

func numericIndex(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case int64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func streamError(payload any) error {
	return newUpstreamStreamError(payload)
}

func aggregateChatCompletionStream(reader io.Reader) (map[string]any, error) {
	completionID := ""
	model := ""
	created := time.Now().Unix()
	choices := map[int]*chatChoiceAccumulator{}
	choiceIndexes := []int{}
	var latestUsage map[string]any

	getChoice := func(index int) *chatChoiceAccumulator {
		choice := choices[index]
		if choice != nil {
			return choice
		}
		choice = &chatChoiceAccumulator{
			index:     index,
			role:      "assistant",
			toolCalls: map[int]*chatToolCallAccumulator{},
		}
		choices[index] = choice
		choiceIndexes = append(choiceIndexes, index)
		return choice
	}

	buffered := bufio.NewReader(reader)
	for {
		line, readErr := buffered.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(line[5:])
				if payload != "" && payload != "[DONE]" {
					var event map[string]any
					if err := json.Unmarshal([]byte(payload), &event); err != nil {
						return nil, fmt.Errorf("decode upstream SSE event: %w", err)
					}
					event = unwrapDataEnvelope(event)
					if errorPayload := event["error"]; errorPayload != nil {
						return nil, streamError(errorPayload)
					}
					if value, _ := event["id"].(string); value != "" {
						completionID = value
					}
					if value, _ := event["model"].(string); value != "" {
						model = value
					}
					if value, ok := event["created"].(float64); ok && value > 0 {
						created = int64(value)
					}
					if usage, ok := event["usage"].(map[string]any); ok {
						latestUsage = usage
					}

					eventChoices, _ := event["choices"].([]any)
					for _, rawChoice := range eventChoices {
						choiceMap, _ := rawChoice.(map[string]any)
						if choiceMap == nil {
							continue
						}
						choice := getChoice(numericIndex(choiceMap["index"]))
						if finishReason, _ := choiceMap["finish_reason"].(string); finishReason != "" {
							choice.finishReason = finishReason
						}
						if logprobs := choiceMap["logprobs"]; logprobs != nil {
							choice.logprobs = logprobs
						}
						delta, _ := choiceMap["delta"].(map[string]any)
						if delta == nil {
							delta, _ = choiceMap["message"].(map[string]any)
						}
						if delta == nil {
							continue
						}
						if role, _ := delta["role"].(string); role != "" {
							choice.role = role
						}
						if content, _ := delta["content"].(string); content != "" {
							choice.content.WriteString(content)
						}
						if refusal, _ := delta["refusal"].(string); refusal != "" {
							choice.refusal.WriteString(refusal)
						}
						reasoning, _ := delta["reasoning_content"].(string)
						if reasoning == "" {
							reasoning, _ = delta["reasoning"].(string)
						}
						if reasoning != "" {
							choice.reasoning.WriteString(reasoning)
						}

						rawToolCalls, _ := delta["tool_calls"].([]any)
						for _, rawToolCall := range rawToolCalls {
							toolCallMap, _ := rawToolCall.(map[string]any)
							if toolCallMap == nil {
								continue
							}
							toolIndex := numericIndex(toolCallMap["index"])
							toolCall := choice.toolCalls[toolIndex]
							if toolCall == nil {
								toolCall = &chatToolCallAccumulator{}
								choice.toolCalls[toolIndex] = toolCall
								choice.toolCallIndexes = append(choice.toolCallIndexes, toolIndex)
							}
							if id, _ := toolCallMap["id"].(string); id != "" {
								toolCall.id = id
							}
							function, _ := toolCallMap["function"].(map[string]any)
							if function == nil {
								continue
							}
							if name, _ := function["name"].(string); name != "" {
								toolCall.name = name
							}
							if arguments, _ := function["arguments"].(string); arguments != "" {
								toolCall.arguments.WriteString(arguments)
							}
						}
					}
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("read upstream SSE: %w", readErr)
			}
			break
		}
	}

	sort.Ints(choiceIndexes)
	outputChoices := make([]any, 0, len(choiceIndexes))
	hasVisibleOutput := false
	for _, choiceIndex := range choiceIndexes {
		choice := choices[choiceIndex]
		message := map[string]any{
			"role":    choice.role,
			"content": choice.content.String(),
		}
		if reasoning := choice.reasoning.String(); reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		if refusal := choice.refusal.String(); refusal != "" {
			message["refusal"] = refusal
			hasVisibleOutput = true
		}
		if strings.TrimSpace(choice.content.String()) != "" {
			hasVisibleOutput = true
		}

		sort.Ints(choice.toolCallIndexes)
		toolCalls := make([]any, 0, len(choice.toolCallIndexes))
		for _, toolIndex := range choice.toolCallIndexes {
			toolCall := choice.toolCalls[toolIndex]
			if toolCall.name == "" {
				continue
			}
			id := toolCall.id
			if id == "" {
				id = newResponseID("call_")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      toolCall.name,
					"arguments": toolCall.arguments.String(),
				},
			})
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
			hasVisibleOutput = true
		}

		finishReason := choice.finishReason
		if finishReason == "" {
			if len(toolCalls) > 0 {
				finishReason = "tool_calls"
			} else {
				finishReason = "stop"
			}
		}
		outputChoices = append(outputChoices, map[string]any{
			"index":         choice.index,
			"message":       message,
			"finish_reason": finishReason,
			"logprobs":      choice.logprobs,
		})
	}

	if !hasVisibleOutput {
		return nil, errEmptyResponseContent
	}
	if completionID == "" {
		completionID = newResponseID("chatcmpl_")
	}
	result := map[string]any{
		"id":      completionID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": outputChoices,
	}
	if latestUsage != nil {
		result["usage"] = latestUsage
	}
	return result, nil
}

func decodeChatCompletionResponse(response *http.Response, streamed bool) (map[string]any, error) {
	if streamed {
		return aggregateChatCompletionStream(response.Body)
	}
	var raw map[string]any
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return normalizeOpenAIResponse(unwrapDataEnvelope(raw)), nil
}

func isEmptyResponseError(err error) bool {
	if errors.Is(err, errEmptyResponseContent) {
		return true
	}
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "empty response content")
}

func callClineNonStream(params map[string]any) (map[string]any, *Account, error) {
	forceStream := shouldForceClineStream(params, false)
	response, account, err := callClineAPI(params, forceStream)
	if err == nil {
		result, decodeErr := decodeChatCompletionResponse(response, forceStream)
		response.Body.Close()
		if decodeErr == nil {
			return result, account, nil
		}
		err = decodeErr
	}
	if !forceStream || !isEmptyResponseError(err) {
		return nil, account, err
	}

	model, _ := params["model"].(string)
	alternative := pickAlternativeAccountForModel(model, account)
	if alternative == nil {
		return nil, account, err
	}
	logMessage := "  upstream stream returned empty content; retrying once with another account"
	if err != nil && !errors.Is(err, errEmptyResponseContent) {
		logMessage = "  upstream returned empty response; retrying once as stream with another account"
	}
	log.Print(logMessage)

	retryResponse, retryAccount, retryErr := callClineAPIWithAccount(alternative, params, true)
	if retryErr != nil {
		return nil, retryAccount, retryErr
	}
	retryResult, retryDecodeErr := decodeChatCompletionResponse(retryResponse, true)
	retryResponse.Body.Close()
	if retryDecodeErr != nil {
		return nil, retryAccount, retryDecodeErr
	}
	return retryResult, retryAccount, nil
}
