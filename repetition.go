package main

import "bytes"

const (
	repetitiveOutputErrorCode  = "repetitive_output"
	repetitionMinEvidenceBytes = 384
	repetitionMinCycles        = 8
	repetitionMinPeriodBytes   = 4
	repetitionMaxPeriodBytes   = 512
	repetitionWindowBytes      = 16 * 1024
	repetitionCheckInterval    = 64
)

// outputRepetitionGuard detects a periodic suffix independently of how the
// upstream split its SSE chunks. It keeps only a small rolling window and
// requires both several complete cycles and enough repeated bytes to avoid
// treating normal short emphasis as a runaway generation.
type outputRepetitionGuard struct {
	window     []byte
	sinceCheck int
}

func (guard *outputRepetitionGuard) Observe(fragment string) bool {
	if fragment == "" {
		return false
	}
	guard.window = append(guard.window, fragment...)
	if len(guard.window) > repetitionWindowBytes {
		start := len(guard.window) - repetitionWindowBytes
		copy(guard.window, guard.window[start:])
		guard.window = guard.window[:repetitionWindowBytes]
	}
	guard.sinceCheck += len(fragment)
	if len(guard.window) < repetitionMinEvidenceBytes || guard.sinceCheck < repetitionCheckInterval {
		return false
	}
	guard.sinceCheck = 0
	return hasRepetitiveSuffix(guard.window)
}

func hasRepetitiveSuffix(output []byte) bool {
	maxPeriod := min(repetitionMaxPeriodBytes, len(output)/repetitionMinCycles)
	for period := repetitionMinPeriodBytes; period <= maxPeriod; period++ {
		cycles := max(repetitionMinCycles, (repetitionMinEvidenceBytes+period-1)/period)
		span := cycles * period
		if span > len(output) {
			continue
		}
		tail := output[len(output)-span:]
		if bytes.Equal(tail[period:], tail[:len(tail)-period]) {
			return true
		}
	}
	return false
}
