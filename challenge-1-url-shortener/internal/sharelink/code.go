package sharelink

import (
	"crypto/rand"
	"fmt"
)

const (
	codeAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	// CodeLength of 8 base62 chars gives 62^8 ≈ 2.18e14 codes. With 10M live
	// links an attacker needs ~2e7 guesses per hit, which rate limiting makes
	// impractical, and insert collisions stay around 1 in 20 million.
	CodeLength = 8

	// 248 is the largest multiple of 62 that fits in a byte; bytes at or above
	// it are rejected so every character is equally likely.
	rejectThreshold = 248
)

// NewCode returns a random, URL-safe share code.
//
// Codes are deliberately not derived from auto-increment ids or hashes of the
// submission id: a share link exposes a student's work, so codes must not be
// enumerable or predictable.
func NewCode() (string, error) {
	out := make([]byte, 0, CodeLength)
	buf := make([]byte, CodeLength*2)
	for len(out) < CodeLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("sharelink: read random bytes: %w", err)
		}
		for _, b := range buf {
			if b >= rejectThreshold {
				continue
			}
			out = append(out, codeAlphabet[b%62])
			if len(out) == CodeLength {
				break
			}
		}
	}
	return string(out), nil
}

// ValidCode rejects malformed codes before they reach the cache or database,
// so garbage traffic on /s/:code costs nothing.
func ValidCode(s string) bool {
	if len(s) != CodeLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	return true
}
