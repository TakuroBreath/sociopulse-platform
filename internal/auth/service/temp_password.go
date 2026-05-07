package service

import (
	"crypto/rand"
	"errors"
	"fmt"
)

// tempPasswordLen is the fixed length of a generated temp password.
// 16 chars × log2(64) = 96 bits of entropy — well above the 80-bit
// "strong random" floor and short enough to type by hand once.
const tempPasswordLen = 16

// tempPasswordAlphabet is a 64-char URL-safe alphabet (RFC 4648 § 5).
// 64 = 2^6 makes byte%64 unbiased: every byte from crypto/rand maps
// to one alphabet index without modulo skew, so every char position
// has uniform probability.
const tempPasswordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// GenerateTempPassword returns a 16-char URL-safe random password
// suitable for the FR-A4 force-change-on-first-login flow. Bytes come
// from crypto/rand (the only source acceptable per
// samber/cc-skills-golang@golang-security § Quick-Reference); a 64-char
// alphabet keeps each char unbiased modulo 64.
//
// Returned password is safe to paste into URLs, JSON, and email bodies
// without escaping — only [A-Za-z0-9_-] characters appear.
func GenerateTempPassword() (string, error) {
	if len(tempPasswordAlphabet) != 64 {
		// Compile-time invariant; surfaced at runtime so a future tweak
		// to the alphabet that breaks the unbiased-modulo property fails
		// loudly instead of silently distorting the entropy.
		return "", errors.New("auth/service: temp password alphabet must be 64 chars for unbiased mod")
	}
	var raw [tempPasswordLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("auth/service: read random bytes for temp password: %w", err)
	}
	out := make([]byte, tempPasswordLen)
	for i, b := range raw {
		out[i] = tempPasswordAlphabet[b%64]
	}
	return string(out), nil
}
