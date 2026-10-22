package main

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func encodeTestHash(t *testing.T, password string, salt []byte) string {
	t.Helper()
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordHashIterations, passwordKeyBytes)
	if err != nil {
		t.Fatalf("derive test hash: %v", err)
	}
	return "$" + passwordHashAlgorithm + "$" +
		"600000" + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key)
}

func TestPasswordHashRoundTrip(t *testing.T) {
	const password = "correct horse battery"

	encoded, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if !strings.HasPrefix(encoded, "$pbkdf2-sha256$600000$") {
		t.Fatalf("unexpected encoded format: %q", encoded)
	}
	if !verifyPassword(encoded, password) {
		t.Fatal("verifyPassword rejected the correct password")
	}
	if verifyPassword(encoded, password+"!") {
		t.Fatal("verifyPassword accepted a wrong password")
	}

	other, err := hashPassword(password)
	if err != nil {
		t.Fatalf("second hashPassword: %v", err)
	}
	encodedSalt := strings.Split(encoded, "$")[3]
	otherSalt := strings.Split(other, "$")[3]
	if encodedSalt == otherSalt {
		t.Fatal("same password produced the same salt")
	}
	if encoded == other {
		t.Fatal("same password produced the same encoded hash")
	}
	if !verifyPassword(other, password) {
		t.Fatal("verifyPassword rejected hash with a different salt")
	}
}

func TestPasswordVerifyAcceptsOldShortPolicy(t *testing.T) {
	const shortPassword = "oldpass"
	salt := make([]byte, passwordSaltBytes)
	for i := range salt {
		salt[i] = byte(i)
	}
	encoded := encodeTestHash(t, shortPassword, salt)

	if !verifyPassword(encoded, shortPassword) {
		t.Fatal("verifyPassword rejected a stored hash created under a shorter policy")
	}
	if verifyPassword(encoded, "oldpas") {
		t.Fatal("verifyPassword accepted a wrong short password")
	}
}

func TestPasswordVerifyRejectsHostileEncoding(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString(make([]byte, passwordSaltBytes))
	key := base64.RawStdEncoding.EncodeToString(make([]byte, passwordKeyBytes))
	paddedSalt := base64.StdEncoding.EncodeToString(make([]byte, passwordSaltBytes))
	shortKey := base64.RawStdEncoding.EncodeToString(make([]byte, passwordKeyBytes-1))

	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"missing fields", "$pbkdf2-sha256$600000$" + salt},
		{"extra field", "$pbkdf2-sha256$600000$" + salt + "$" + key + "$extra"},
		{"missing leading separator", "pbkdf2-sha256$600000$" + salt + "$" + key},
		{"unknown algorithm", "$scrypt$600000$" + salt + "$" + key},
		{"one iteration", "$pbkdf2-sha256$1$" + salt + "$" + key},
		{"excessive iterations", "$pbkdf2-sha256$100000000$" + salt + "$" + key},
		{"non-numeric iterations", "$pbkdf2-sha256$many$" + salt + "$" + key},
		{"zero-padded iterations", "$pbkdf2-sha256$0600000$" + salt + "$" + key},
		{"short salt", "$pbkdf2-sha256$600000$AAAA$" + key},
		{"padded salt", "$pbkdf2-sha256$600000$" + paddedSalt + "$" + key},
		{"invalid salt base64", "$pbkdf2-sha256$600000$!!!!$" + key},
		{"short key", "$pbkdf2-sha256$600000$" + salt + "$" + shortKey},
		{"invalid key base64", "$pbkdf2-sha256$600000$" + salt + "$!!!!"},
		{"overlong encoding", strings.Repeat("a", passwordEncodedMaxBytes+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if verifyPassword(test.encoded, "correct horse battery") {
				t.Fatalf("verifyPassword accepted hostile encoding %q", test.encoded)
			}
		})
	}
}

func TestPasswordVerifyRejectsOverlongPassword(t *testing.T) {
	const password = "correct horse battery"
	encoded, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}

	if verifyPassword(encoded, strings.Repeat("a", passwordMaxBytes+1)) {
		t.Fatal("verifyPassword accepted a password over the byte limit")
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"empty", "", true},
		{"eleven ASCII code points", "abcdefghijk", true},
		{"twelve ASCII code points", "abcdefghijkl", false},
		{"eleven Unicode code points", strings.Repeat("é", 11), true},
		{"twelve Unicode code points", strings.Repeat("é", 12), false},
		{"exactly maximum bytes", strings.Repeat("a", passwordMaxBytes), false},
		{"maximum multibyte bytes", strings.Repeat("é", passwordMaxBytes/2), false},
		{"over maximum bytes", strings.Repeat("a", passwordMaxBytes+1), true},
		{"over maximum multibyte bytes", strings.Repeat("é", passwordMaxBytes/2+1), true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validatePassword(test.password)
			if test.wantErr && err == nil {
				t.Fatal("validatePassword accepted an invalid password")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validatePassword rejected a valid password: %v", err)
			}
		})
	}
}
