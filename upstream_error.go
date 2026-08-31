package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var errStreamEarlyEOF = errors.New("upstream stream ended before a terminal event")

var embeddedHTTPStatusPattern = regexp.MustCompile(`(?i)status\s+([45][0-9]{2})`)

type upstreamResponseError struct {
	message    string
	statusCode int
}

func (err *upstreamResponseError) Error() string {
	return err.message
}

func newUpstreamHTTPError(statusCode int, body string) error {
	return &upstreamResponseError{
		message:    fmt.Sprintf("API %d: %s", statusCode, truncate(body, 500)),
		statusCode: statusCode,
	}
}

func newUpstreamStreamError(payload any) error {
	encoded, encodeErr := json.Marshal(payload)
	if encodeErr != nil {
		return &upstreamResponseError{message: "upstream stream error"}
	}
	return &upstreamResponseError{
		message:    "upstream stream error: " + truncate(string(encoded), 500),
		statusCode: inferEmbeddedHTTPStatus(payload),
	}
}

func inferEmbeddedHTTPStatus(value any) int {
	switch item := value.(type) {
	case map[string]any:
		if status := numericHTTPStatus(item["status"]); status != 0 {
			return status
		}
		if status := numericHTTPStatus(item["status_code"]); status != 0 {
			return status
		}
		if status := numericHTTPStatus(item["code"]); status != 0 {
			return status
		}
		for _, nested := range []string{"error", "message", "metadata", "raw"} {
			if status := inferEmbeddedHTTPStatus(item[nested]); status != 0 {
				return status
			}
		}
	case []any:
		for _, nested := range item {
			if status := inferEmbeddedHTTPStatus(nested); status != 0 {
				return status
			}
		}
	case string:
		match := embeddedHTTPStatusPattern.FindStringSubmatch(item)
		if len(match) == 2 {
			status, _ := strconv.Atoi(match[1])
			return status
		}
	}
	return 0
}

func numericHTTPStatus(value any) int {
	status := int(tokenCount(value))
	if status >= 400 && status <= 599 {
		return status
	}
	if text, ok := value.(string); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(text))
		if err == nil && parsed >= 400 && parsed <= 599 {
			return parsed
		}
	}
	return 0
}

func upstreamErrorStatus(err error) int {
	var upstreamErr *upstreamResponseError
	if errors.As(err, &upstreamErr) {
		return upstreamErr.statusCode
	}
	return 0
}

func retryableResponsesInitializationError(err error) bool {
	if errors.Is(err, errStreamEarlyEOF) || isEmptyResponseError(err) {
		return true
	}
	status := upstreamErrorStatus(err)
	return status == 429 || status >= 500
}

func responsesUpstreamErrorDetails(err error) (int, string, string) {
	status := upstreamErrorStatus(err)
	switch {
	case status == 429:
		return 429, "rate_limit_error", "rate_limit_exceeded"
	case status >= 400 && status < 500:
		return status, "invalid_request_error", "upstream_invalid_request"
	case errors.Is(err, errStreamEarlyEOF):
		return 502, "api_error", "stream_early_eof"
	default:
		return 502, "api_error", "upstream_error"
	}
}
