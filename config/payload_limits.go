package config

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// DefaultMaxPayloadBytes is the conservative serialized ceiling for a converted
// Kiro request body. Kiro rejects an oversized request with HTTP 400
// "Input is too long." (CONTENT_LENGTH_EXCEEDS_THRESHOLD) and names neither the
// size nor the limit, so the default stays below the observed threshold.
const DefaultMaxPayloadBytes = 900 * 1024

// MinConfigurableMaxPayloadBytes guards against a cap so small that every
// request would be trimmed down to the current message.
const MinConfigurableMaxPayloadBytes = 64 * 1024

// MaxPayloadBytesEnv is the environment variable that overrides the ceiling.
const MaxPayloadBytesEnv = "KIRO_MAX_PAYLOAD_BYTES"

var (
	maxPayloadBytesOnce  sync.Once
	maxPayloadBytesValue int
)

// MaxPayloadBytes returns the serialized byte ceiling for a converted Kiro
// payload. It is resolved once from KIRO_MAX_PAYLOAD_BYTES:
//
//	unset or empty          -> DefaultMaxPayloadBytes
//	0                       -> no byte ceiling (token limits still apply)
//	>= MinConfigurableMaxPayloadBytes -> the supplied value
//	anything else           -> DefaultMaxPayloadBytes
//
// The upstream reject boundary is not a wire-byte count, so a deployment that
// has measured its own threshold can raise or disable this cap. Token limits
// derived from the model's context window are enforced independently.
func MaxPayloadBytes() int {
	maxPayloadBytesOnce.Do(func() {
		maxPayloadBytesValue = resolveMaxPayloadBytes(os.Getenv(MaxPayloadBytesEnv))
	})
	return maxPayloadBytesValue
}

func resolveMaxPayloadBytes(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultMaxPayloadBytes
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return DefaultMaxPayloadBytes
	}
	if parsed == 0 {
		return 0
	}
	if parsed < MinConfigurableMaxPayloadBytes {
		return DefaultMaxPayloadBytes
	}
	return parsed
}
