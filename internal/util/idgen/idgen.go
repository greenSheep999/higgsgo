// Package idgen produces short, url-safe, sortable identifiers.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewID returns a prefixed random id like "key-9f2a3e...".
// 8 hex chars of timestamp (seconds) + 12 hex chars of randomness → 20 chars body.
// The timestamp prefix makes rows visually sortable by creation time.
func NewID(prefix string) string {
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	ts := uint32(time.Now().Unix())
	return fmt.Sprintf("%s_%08x%s", prefix, ts, hex.EncodeToString(buf[:]))
}
