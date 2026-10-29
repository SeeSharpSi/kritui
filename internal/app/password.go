package app

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	passwordHashAlgorithm  = "pbkdf2-sha256"
	passwordHashIterations = 600000
	passwordSaltBytes      = 16
	passwordKeyBytes       = 32
	passwordMinRunes       = 12
	passwordMaxBytes       = 1024

	// passwordEncodedMaxBytes bounds parsing work before any splitting or
	// decoding for values that could never be a valid encoded hash.
	passwordEncodedMaxBytes = 256
)

// hashPassword derives a salted PBKDF2-HMAC-SHA256 hash for a new password.
// The returned value uses the format
// $pbkdf2-sha256$600000$<salt>$<key> with unpadded standard base64 fields.
func hashPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}

	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}

	key, err := pbkdf2.Key(sha256.New, password, salt, passwordHashIterations, passwordKeyBytes)
	if err != nil {
		return "", fmt.Errorf("derive password hash: %w", err)
	}

	return fmt.Sprintf(
		"$%s$%d$%s$%s",
		passwordHashAlgorithm,
		passwordHashIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// verifyPassword checks a password against a hash produced by hashPassword.
// It accepts passwords that are shorter than the current policy so hashes
// created under an older policy can still be checked. Verification work is
// bounded: the iteration count, salt size, and key size in encoded must all
// match the current parameters before key derivation starts.
func verifyPassword(encoded, password string) bool {
	if len(password) > passwordMaxBytes || len(encoded) > passwordEncodedMaxBytes {
		return false
	}

	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != passwordHashAlgorithm {
		return false
	}
	if parts[2] != fmt.Sprintf("%d", passwordHashIterations) {
		return false
	}

	saltEncodedLen := base64.RawStdEncoding.EncodedLen(passwordSaltBytes)
	keyEncodedLen := base64.RawStdEncoding.EncodedLen(passwordKeyBytes)
	if len(parts[3]) != saltEncodedLen || len(parts[4]) != keyEncodedLen {
		return false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) != passwordSaltBytes {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(expected) != passwordKeyBytes {
		return false
	}

	derived, err := pbkdf2.Key(sha256.New, password, salt, passwordHashIterations, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(derived, expected) == 1
}

// validatePassword enforces the current password policy: at least 12 Unicode
// code points and no more than 1024 UTF-8 bytes.
func validatePassword(password string) error {
	if password == "" {
		return errors.New("password is required")
	}
	if len(password) > passwordMaxBytes {
		return fmt.Errorf("password must be at most %d bytes", passwordMaxBytes)
	}
	if utf8.RuneCountInString(password) < passwordMinRunes {
		return fmt.Errorf("password must be at least %d characters", passwordMinRunes)
	}
	return nil
}
