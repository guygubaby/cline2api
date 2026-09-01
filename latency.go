package main

import (
	"errors"
	"net/http"
	"time"
)

const (
	latencyEWMAAlpha      = 0.25
	slowChannelAbsoluteMs = 10_000
	slowChannelMarginMs   = 3_000
	slowChannelRatio      = 2
	slowChannelCooldown   = 30 * time.Second
)

func validLoadBalancingStrategy(strategy string) bool {
	switch strategy {
	case "round_robin", "fill", "random", "least_latency":
		return true
	default:
		return false
	}
}

func observeModelLatency(stat *ModelLatencyStat, elapsed time.Duration) {
	observedMs := float64(elapsed.Milliseconds())
	if observedMs <= 0 {
		return
	}
	if stat.Samples == 0 || stat.EWMAms <= 0 {
		stat.EWMAms = observedMs
	} else {
		stat.EWMAms = latencyEWMAAlpha*observedMs + (1-latencyEWMAAlpha)*stat.EWMAms
	}
	stat.Samples++
}

func anomalouslySlowLatency(observed time.Duration, bestAlternativeMs float64) bool {
	observedMs := float64(observed.Milliseconds())
	return bestAlternativeMs > 0 &&
		observedMs >= slowChannelAbsoluteMs &&
		observedMs-bestAlternativeMs >= slowChannelMarginMs &&
		observedMs >= bestAlternativeMs*slowChannelRatio
}

func recordUpstreamTTFT(params map[string]any, elapsed time.Duration) {
	if params == nil || elapsed <= 0 {
		return
	}
	params[proxyUpstreamTTFTParamKey] = elapsed.Milliseconds()
}

func recordSuccessfulAccountAttempt(params map[string]any, account *Account, model string, attemptElapsed, totalElapsed time.Duration) {
	recordUpstreamTTFT(params, totalElapsed)
	observeAccountModelLatency(account, model, attemptElapsed)
	coolSlowAccountIfNeeded(account, model, attemptElapsed)
}

func coolAccountAfterStreamInitializationError(account *Account, model string, err error) {
	if account == nil {
		return
	}
	switch {
	case errors.Is(err, errUpstreamFirstEventTimeout):
		setModelCooldown(account, model, time.Now().Add(slowAccountModelCooldown))
	case upstreamErrorStatus(err) == http.StatusTooManyRequests:
		setModelCooldown(account, model, time.Now().Add(5*time.Minute))
	}
}

func recordRequestLatencyPhases(entry *RequestLog, eventAt time.Time, thinking, visible bool) {
	if entry == nil || (!thinking && !visible) {
		return
	}
	elapsedMs := eventAt.Sub(entry.StartedAt).Milliseconds()
	if entry.UpstreamTTFTMs == 0 {
		entry.UpstreamTTFTMs = elapsedMs
	}
	if thinking && entry.ThinkingTTFTMs == 0 {
		entry.ThinkingTTFTMs = elapsedMs
	}
	if visible && entry.VisibleTTFTMs == 0 {
		entry.VisibleTTFTMs = elapsedMs
	}
}
