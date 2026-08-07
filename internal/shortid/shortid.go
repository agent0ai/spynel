// Package shortid formats opaque identifiers for compact human-facing output.
package shortid

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

const displayLength = 8

// New creates a compact random identifier suitable for Spynel-owned files and
// job-like handles. Provider-owned opaque IDs must never be replaced by it.
func New() (string, error) {
	data := make([]byte, 5)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(data)), nil
}

// Display returns at most eight alphanumeric characters from an opaque ID.
// Callers must retain the original value for persistence and protocol calls.
func Display(value string) string {
	result := make([]rune, 0, displayLength)
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9')) {
			continue
		}
		result = append(result, character)
		if len(result) == displayLength {
			break
		}
	}
	return string(result)
}
