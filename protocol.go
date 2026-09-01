package main

import (
	"errors"
	"net/http"
	"time"
)

func writeOpenAIError(w http.ResponseWriter, status int, errorType, message string) {
	writeOpenAIErrorCode(w, status, errorType, nil, message)
}

func writeOpenAIErrorCode(w http.ResponseWriter, status int, errorType string, errorCode any, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"param":   nil,
			"code":    errorCode,
		},
	})
}

func writeAnthropicError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	})
}

func writeAnthropicUpstreamError(w http.ResponseWriter, reqLog *RequestLog, usage tokenUsage, err error) {
	status, errorType := anthropicUpstreamErrorDetails(err)
	finalizeRequestLog(reqLog, usage, time.Time{}, reqLog.StartedAt, false, err.Error())
	writeAnthropicError(w, status, errorType, err.Error())
}

func anthropicUpstreamErrorDetails(err error) (int, string) {
	status := http.StatusBadGateway
	errorType := "api_error"
	if upstreamErrorStatus(err) == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
		errorType = "rate_limit_error"
	} else if errors.Is(err, errUpstreamFirstEventTimeout) {
		status = http.StatusGatewayTimeout
	}
	return status, errorType
}
