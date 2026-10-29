package kritui_db

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func newTestStorageCipher(t *testing.T) *StorageCipher {
	t.Helper()
	key := make([]byte, StorageKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	storage, err := NewStorageCipher(key)
	if err != nil {
		t.Fatalf("NewStorageCipher() error: %v", err)
	}
	return storage
}

func testStorageKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, StorageKeySize)
	for i := range key {
		key[i] = byte(255 - i)
	}
	return key
}

func mustSealStorage(t *testing.T, storage *StorageCipher, plaintext []byte, scope string, ownerID int64, rowID string) []byte {
	t.Helper()
	envelope, err := storage.Seal(plaintext, scope, ownerID, rowID)
	if err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
	return envelope
}

func TestNewStorageCipherKeyLengths(t *testing.T) {
	for _, size := range []int{0, 1, 16, 24, 31, 33, 64} {
		key := make([]byte, size)
		if _, err := NewStorageCipher(key); err == nil {
			t.Errorf("NewStorageCipher(%d bytes) error = nil, want error", size)
		}
	}
	if _, err := NewStorageCipher(nil); err == nil {
		t.Error("NewStorageCipher(nil) error = nil, want error")
	}
	if _, err := NewStorageCipher(testStorageKey(t)); err != nil {
		t.Errorf("NewStorageCipher(32 bytes) error: %v", err)
	}
}

func TestNewStorageCipherCopiesKey(t *testing.T) {
	key := testStorageKey(t)
	storage, err := NewStorageCipher(key)
	if err != nil {
		t.Fatalf("NewStorageCipher() error: %v", err)
	}
	envelope := mustSealStorage(t, storage, []byte("secret"), "settings.api_key", 4, "row-1")
	clear(key)
	same, err := NewStorageCipher(testStorageKey(t))
	if err != nil {
		t.Fatalf("NewStorageCipher() second error: %v", err)
	}
	plaintext, err := same.Open(envelope, "settings.api_key", 4, "row-1")
	if err != nil {
		t.Fatalf("Open() after clearing source key error: %v", err)
	}
	if string(plaintext) != "secret" {
		t.Fatalf("Open() = %q, want %q", plaintext, "secret")
	}
}

func TestStorageCipherSealOpenRoundTrip(t *testing.T) {
	storage := newTestStorageCipher(t)
	texts := []string{
		"",
		"hello",
		"h\u00e9llo \u4e16\u754c \U0001f680",
		"line\nbreak\x00null",
		strings.Repeat("abc", 1000),
	}
	for i, value := range texts {
		t.Run("text/case-"+strconv.Itoa(i), func(t *testing.T) {
			envelope := mustSealStorage(t, storage, []byte(value), "messages.content", 7, "42")
			if string(envelope) == value {
				t.Fatal("Seal() returned plaintext")
			}
			plaintext, err := storage.Open(envelope, "messages.content", 7, "42")
			if err != nil {
				t.Fatalf("Open() error: %v", err)
			}
			if string(plaintext) != value {
				t.Fatalf("Open() = %q, want %q", plaintext, value)
			}
		})
	}

	binaries := [][]byte{
		{},
		{0x00, 0xff, 0x80, 0x01, 0x00},
		bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 512),
	}
	for _, value := range binaries {
		envelope := mustSealStorage(t, storage, value, "messages.attachment", 9, "blob")
		plaintext, err := storage.Open(envelope, "messages.attachment", 9, "blob")
		if err != nil {
			t.Fatalf("Open() error: %v", err)
		}
		if !bytes.Equal(plaintext, value) {
			t.Fatalf("Open() = %x, want %x", plaintext, value)
		}
	}
}

func TestStorageCipherUsesFreshNonce(t *testing.T) {
	storage := newTestStorageCipher(t)
	first := mustSealStorage(t, storage, []byte("same input"), "settings.value", 3, "row")
	second := mustSealStorage(t, storage, []byte("same input"), "settings.value", 3, "row")
	if bytes.Equal(first, second) {
		t.Fatal("Seal() reused an envelope for identical input")
	}
	for _, envelope := range [][]byte{first, second} {
		plaintext, err := storage.Open(envelope, "settings.value", 3, "row")
		if err != nil {
			t.Fatalf("Open() error: %v", err)
		}
		if string(plaintext) != "same input" {
			t.Fatalf("Open() = %q, want %q", plaintext, "same input")
		}
	}
}

func TestStorageCipherBindsAdditionalData(t *testing.T) {
	storage := newTestStorageCipher(t)
	envelope := mustSealStorage(t, storage, []byte("bound"), "settings.api_key", 5, "row-a")
	matches := []struct {
		name    string
		scope   string
		ownerID int64
		rowID   string
	}{
		{name: "different owner", scope: "settings.api_key", ownerID: 6, rowID: "row-a"},
		{name: "different scope", scope: "settings.llm_key", ownerID: 5, rowID: "row-a"},
		{name: "different row", scope: "settings.api_key", ownerID: 5, rowID: "row-b"},
		{name: "scope prefix", scope: "settings.api_ke", ownerID: 5, rowID: "row-a"},
		{name: "scope suffix", scope: "settings.api_key.extra", ownerID: 5, rowID: "row-a"},
		{name: "row prefix", scope: "settings.api_key", ownerID: 5, rowID: "row-"},
		{name: "row suffix", scope: "settings.api_key", ownerID: 5, rowID: "row-a2"},
	}
	for _, tc := range matches {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := storage.Open(envelope, tc.scope, tc.ownerID, tc.rowID); !errors.Is(err, ErrStorageDecryption) {
				t.Fatalf("Open() error = %v, want ErrStorageDecryption", err)
			}
		})
	}
	plaintext, err := storage.Open(envelope, "settings.api_key", 5, "row-a")
	if err != nil {
		t.Fatalf("Open() with matching binding error: %v", err)
	}
	if string(plaintext) != "bound" {
		t.Fatalf("Open() = %q, want %q", plaintext, "bound")
	}
}

func TestStorageCipherRejectsTamperedEnvelope(t *testing.T) {
	storage := newTestStorageCipher(t)
	envelope := mustSealStorage(t, storage, []byte("tamper me"), "settings.value", 2, "row")
	for i := range envelope {
		tampered := bytes.Clone(envelope)
		tampered[i] ^= 0x01
		if _, err := storage.Open(tampered, "settings.value", 2, "row"); !errors.Is(err, ErrStorageDecryption) {
			t.Errorf("Open() after flipping byte %d error = %v, want ErrStorageDecryption", i, err)
		}
	}
}

func TestStorageCipherRejectsMalformedEnvelope(t *testing.T) {
	storage := newTestStorageCipher(t)
	envelope := mustSealStorage(t, storage, []byte("malformed"), "settings.value", 2, "row")
	minimum := storageHeaderSize + storageTagSize

	truncated := []int{0, 1, len(storageMagic) - 1, len(storageMagic), storageHeaderSize - 1, storageHeaderSize, minimum - 1, len(envelope) - 1}
	for _, length := range truncated {
		if _, err := storage.Open(envelope[:length], "settings.value", 2, "row"); !errors.Is(err, ErrStorageDecryption) {
			t.Errorf("Open() with length %d error = %v, want ErrStorageDecryption", length, err)
		}
	}

	appended := append(bytes.Clone(envelope), 0x00)
	if _, err := storage.Open(appended, "settings.value", 2, "row"); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("Open() with appended byte error = %v, want ErrStorageDecryption", err)
	}

	unknownVersion := bytes.Clone(envelope)
	unknownVersion[len(storageMagic)] = storageVersion + 1
	if _, err := storage.Open(unknownVersion, "settings.value", 2, "row"); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("Open() with unknown version error = %v, want ErrStorageDecryption", err)
	}

	badMagic := bytes.Clone(envelope)
	badMagic[0] ^= 0xff
	if _, err := storage.Open(badMagic, "settings.value", 2, "row"); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("Open() with bad magic error = %v, want ErrStorageDecryption", err)
	}

	for _, plaintext := range [][]byte{nil, {}, []byte("plain"), []byte(storageMagic), bytes.Repeat([]byte{0}, minimum)} {
		if _, err := storage.Open(plaintext, "settings.value", 2, "row"); !errors.Is(err, ErrStorageDecryption) {
			t.Errorf("Open() on plaintext %x error = %v, want ErrStorageDecryption", plaintext, err)
		}
	}
}

func TestStorageCipherZeroAndNilFailSafely(t *testing.T) {
	valid := mustSealStorage(t, newTestStorageCipher(t), []byte("value"), "settings.value", 1, "row")
	var zero StorageCipher
	if _, err := zero.Seal([]byte("value"), "settings.value", 1, "row"); err == nil {
		t.Error("zero StorageCipher Seal() error = nil, want error")
	}
	if _, err := zero.Open(valid, "settings.value", 1, "row"); err == nil {
		t.Error("zero StorageCipher Open() error = nil, want error")
	}

	var nilCipher *StorageCipher
	if _, err := nilCipher.Seal([]byte("value"), "settings.value", 1, "row"); err == nil {
		t.Error("nil StorageCipher Seal() error = nil, want error")
	}
	if _, err := nilCipher.Open(valid, "settings.value", 1, "row"); err == nil {
		t.Error("nil StorageCipher Open() error = nil, want error")
	}

	ctx := WithStorageCipher(context.Background(), &zero)
	if _, err := EncryptStorageText(ctx, "settings.value", "row", "value"); err == nil {
		t.Error("EncryptStorageText() with zero cipher error = nil, want error")
	}
	if _, err := DecryptStorageText(ctx, "settings.value", "row", "value"); err == nil {
		t.Error("DecryptStorageText() with zero cipher error = nil, want error")
	}
}

func TestEncryptDecryptStorageText(t *testing.T) {
	storage := newTestStorageCipher(t)
	ctx := WithStorageCipher(WithUser(context.Background(), 11), storage)

	values := []string{"", "secret", "h\u00e9llo \u4e16\u754c", "line\nbreak"}
	for _, value := range values {
		encrypted, err := EncryptStorageText(ctx, "settings.api_key", "row-7", value)
		if err != nil {
			t.Fatalf("EncryptStorageText(%q) error: %v", value, err)
		}
		if !strings.HasPrefix(encrypted, storageTextPrefix) {
			t.Fatalf("EncryptStorageText(%q) = %q, want prefix %q", value, encrypted, storageTextPrefix)
		}
		if encrypted == value {
			t.Fatalf("EncryptStorageText(%q) returned plaintext", value)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(encrypted, storageTextPrefix))
		if err != nil {
			t.Fatalf("text envelope base64 error: %v", err)
		}
		if string(raw[:len(storageMagic)]) != storageMagic {
			t.Fatalf("text envelope magic = %q, want %q", raw[:len(storageMagic)], storageMagic)
		}
		if raw[len(storageMagic)] != storageVersion {
			t.Fatalf("text envelope version = %d, want %d", raw[len(storageMagic)], storageVersion)
		}

		decrypted, err := DecryptStorageText(ctx, "settings.api_key", "row-7", encrypted)
		if err != nil {
			t.Fatalf("DecryptStorageText(%q) error: %v", value, err)
		}
		if decrypted != value {
			t.Fatalf("DecryptStorageText() = %q, want %q", decrypted, value)
		}
	}

	first, err := EncryptStorageText(ctx, "settings.api_key", "row-7", "same")
	if err != nil {
		t.Fatalf("EncryptStorageText() error: %v", err)
	}
	second, err := EncryptStorageText(ctx, "settings.api_key", "row-7", "same")
	if err != nil {
		t.Fatalf("EncryptStorageText() second error: %v", err)
	}
	if first == second {
		t.Fatal("EncryptStorageText() reused ciphertext for identical input")
	}
}

func TestDecryptStorageTextRejectsPlaintextAndBadEnvelopes(t *testing.T) {
	storage := newTestStorageCipher(t)
	ctx := WithStorageCipher(WithUser(context.Background(), 11), storage)

	for _, value := range []string{"", "plain", "enc:", "enc:v2:AAAA", "enc:v1:", "enc:v1:not base64", "enc:v1:!!!!"} {
		if _, err := DecryptStorageText(ctx, "settings.api_key", "row", value); !errors.Is(err, ErrStorageDecryption) {
			t.Errorf("DecryptStorageText(%q) error = %v, want ErrStorageDecryption", value, err)
		}
	}

	valid := mustSealStorage(t, storage, []byte("bound"), "settings.api_key", 11, "row")
	encoded := storageTextPrefix + base64.StdEncoding.EncodeToString(valid)
	for _, ctx := range []context.Context{
		WithStorageCipher(WithUser(context.Background(), 12), storage),
		WithStorageCipher(context.Background(), storage),
	} {
		if _, err := DecryptStorageText(ctx, "settings.api_key", "row", encoded); !errors.Is(err, ErrStorageDecryption) {
			t.Errorf("DecryptStorageText() with different owner error = %v, want ErrStorageDecryption", err)
		}
	}
	for _, tc := range []struct{ scope, row string }{
		{"settings.llm_key", "row"},
		{"settings.api_key", "other-row"},
	} {
		if _, err := DecryptStorageText(ctx, tc.scope, tc.row, encoded); !errors.Is(err, ErrStorageDecryption) {
			t.Errorf("DecryptStorageText(scope=%q, row=%q) error = %v, want ErrStorageDecryption", tc.scope, tc.row, err)
		}
	}

	unknownVersion := storageTextPrefix + base64.StdEncoding.EncodeToString(func() []byte {
		envelope := bytes.Clone(valid)
		envelope[len(storageMagic)] = storageVersion + 1
		return envelope
	}())
	if _, err := DecryptStorageText(ctx, "settings.api_key", "row", unknownVersion); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("DecryptStorageText() with unknown version error = %v, want ErrStorageDecryption", err)
	}
}

func TestEncryptDecryptStorageBytes(t *testing.T) {
	storage := newTestStorageCipher(t)
	ctx := WithStorageCipher(WithUser(context.Background(), 13), storage)

	values := [][]byte{{}, []byte("secret"), {0x00, 0xff, 0x80}, bytes.Repeat([]byte{0x42}, 256)}
	for _, value := range values {
		encrypted, err := EncryptStorageBytes(ctx, "messages.attachment", "row-3", value)
		if err != nil {
			t.Fatalf("EncryptStorageBytes() error: %v", err)
		}
		if bytes.Equal(encrypted, value) {
			t.Fatalf("EncryptStorageBytes(%x) returned plaintext", value)
		}
		if string(encrypted[:len(storageMagic)]) != storageMagic {
			t.Fatalf("bytes envelope magic = %q, want %q", encrypted[:len(storageMagic)], storageMagic)
		}
		decrypted, err := DecryptStorageBytes(ctx, "messages.attachment", "row-3", encrypted)
		if err != nil {
			t.Fatalf("DecryptStorageBytes() error: %v", err)
		}
		if !bytes.Equal(decrypted, value) {
			t.Fatalf("DecryptStorageBytes() = %x, want %x", decrypted, value)
		}
	}

	first, err := EncryptStorageBytes(ctx, "messages.attachment", "row-3", []byte("same"))
	if err != nil {
		t.Fatalf("EncryptStorageBytes() error: %v", err)
	}
	second, err := EncryptStorageBytes(ctx, "messages.attachment", "row-3", []byte("same"))
	if err != nil {
		t.Fatalf("EncryptStorageBytes() second error: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("EncryptStorageBytes() reused ciphertext for identical input")
	}

	for _, plaintext := range [][]byte{nil, {}, []byte("plain"), []byte(storageMagic)} {
		if _, err := DecryptStorageBytes(ctx, "messages.attachment", "row-3", plaintext); !errors.Is(err, ErrStorageDecryption) {
			t.Errorf("DecryptStorageBytes(%x) error = %v, want ErrStorageDecryption", plaintext, err)
		}
	}
	if _, err := DecryptStorageBytes(WithStorageCipher(WithUser(context.Background(), 14), storage), "messages.attachment", "row-3", first); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("DecryptStorageBytes() with different owner error = %v, want ErrStorageDecryption", err)
	}
	if _, err := DecryptStorageBytes(ctx, "messages.attachment", "row-4", first); !errors.Is(err, ErrStorageDecryption) {
		t.Errorf("DecryptStorageBytes() with different row error = %v, want ErrStorageDecryption", err)
	}
}

func TestStorageContextHelpersLegacyDirectCallers(t *testing.T) {
	texts := []string{"", "plain", "enc:v1:AAAA", "h\u00e9llo"}
	for _, ctx := range []context.Context{nil, context.Background(), WithUser(context.Background(), LegacyOwnerID)} {
		for _, value := range texts {
			encrypted, err := EncryptStorageText(ctx, "settings.api_key", "row", value)
			if err != nil {
				t.Fatalf("EncryptStorageText() without cipher error: %v", err)
			}
			if encrypted != value {
				t.Fatalf("EncryptStorageText() = %q, want unchanged %q", encrypted, value)
			}
			decrypted, err := DecryptStorageText(ctx, "settings.api_key", "row", value)
			if err != nil {
				t.Fatalf("DecryptStorageText() without cipher error: %v", err)
			}
			if decrypted != value {
				t.Fatalf("DecryptStorageText() = %q, want unchanged %q", decrypted, value)
			}
		}

		for _, value := range [][]byte{{}, []byte("plain"), {0x00, 0xff}} {
			encrypted, err := EncryptStorageBytes(ctx, "messages.attachment", "row", value)
			if err != nil {
				t.Fatalf("EncryptStorageBytes() without cipher error: %v", err)
			}
			if !bytes.Equal(encrypted, value) {
				t.Fatalf("EncryptStorageBytes() = %x, want unchanged %x", encrypted, value)
			}
			decrypted, err := DecryptStorageBytes(ctx, "messages.attachment", "row", value)
			if err != nil {
				t.Fatalf("DecryptStorageBytes() without cipher error: %v", err)
			}
			if !bytes.Equal(decrypted, value) {
				t.Fatalf("DecryptStorageBytes() = %x, want unchanged %x", decrypted, value)
			}
		}
	}
}

const storageRequirementTestEnv = "KRITUI_TEST_STORAGE_ENCRYPTION_REQUIREMENT"

// TestStorageEncryptionRequirementIsOneWay runs the guard checks in a separate
// test process so RequireStorageEncryption cannot affect the parent package
// test process and its established direct raw-SQL tests.
func TestStorageEncryptionRequirementIsOneWay(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestStorageEncryptionRequirementHelper$", "-test.v")
	cmd.Env = append(os.Environ(), storageRequirementTestEnv+"=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("storage encryption requirement helper failed: %v\n%s", err, output)
	}
}

func TestStorageEncryptionRequirementHelper(t *testing.T) {
	if os.Getenv(storageRequirementTestEnv) != "1" {
		t.Skip("subprocess helper; run by TestStorageEncryptionRequirementIsOneWay")
	}

	storage := newTestStorageCipher(t)
	userCtx := WithUser(context.Background(), 21)
	cipherCtx := WithStorageCipher(userCtx, storage)

	legacyText := "legacy plaintext"
	if got, err := EncryptStorageText(userCtx, "settings.api_key", "row", legacyText); err != nil || got != legacyText {
		t.Fatalf("before requirement EncryptStorageText() = %q, %v; want legacy pass-through", got, err)
	}
	if got, err := DecryptStorageText(userCtx, "settings.api_key", "row", legacyText); err != nil || got != legacyText {
		t.Fatalf("before requirement DecryptStorageText() = %q, %v; want legacy pass-through", got, err)
	}
	legacyBytes := []byte{0x00, 0xff, 0x80}
	if got, err := EncryptStorageBytes(userCtx, "messages.attachment", "row", legacyBytes); err != nil || !bytes.Equal(got, legacyBytes) {
		t.Fatalf("before requirement EncryptStorageBytes() = %x, %v; want legacy pass-through", got, err)
	}
	if got, err := DecryptStorageBytes(userCtx, "messages.attachment", "row", legacyBytes); err != nil || !bytes.Equal(got, legacyBytes) {
		t.Fatalf("before requirement DecryptStorageBytes() = %x, %v; want legacy pass-through", got, err)
	}

	RequireStorageEncryption()
	RequireStorageEncryption()

	for _, ctx := range []context.Context{nil, context.Background(), userCtx} {
		if got, err := EncryptStorageText(ctx, "settings.api_key", "row", "must not leak"); !errors.Is(err, errStorageCipherUnavailable) || got != "" {
			t.Fatalf("after requirement EncryptStorageText() = %q, %v; want empty output and errStorageCipherUnavailable", got, err)
		}
		if got, err := DecryptStorageText(ctx, "settings.api_key", "row", "must not leak"); !errors.Is(err, errStorageCipherUnavailable) || got != "" {
			t.Fatalf("after requirement DecryptStorageText() = %q, %v; want empty output and errStorageCipherUnavailable", got, err)
		}
		if got, err := EncryptStorageBytes(ctx, "messages.attachment", "row", []byte("must not leak")); !errors.Is(err, errStorageCipherUnavailable) || len(got) != 0 {
			t.Fatalf("after requirement EncryptStorageBytes() = %x, %v; want empty output and errStorageCipherUnavailable", got, err)
		}
		if got, err := DecryptStorageBytes(ctx, "messages.attachment", "row", []byte("must not leak")); !errors.Is(err, errStorageCipherUnavailable) || len(got) != 0 {
			t.Fatalf("after requirement DecryptStorageBytes() = %x, %v; want empty output and errStorageCipherUnavailable", got, err)
		}
	}

	nilCipherCtx := WithStorageCipher(userCtx, nil)
	if got, err := EncryptStorageText(nilCipherCtx, "settings.api_key", "row", "must not leak"); !errors.Is(err, errStorageCipherUnavailable) || got != "" {
		t.Fatalf("nil cipher EncryptStorageText() = %q, %v; want empty output and errStorageCipherUnavailable", got, err)
	}
	if got, err := DecryptStorageText(nilCipherCtx, "settings.api_key", "row", "must not leak"); !errors.Is(err, errStorageCipherUnavailable) || got != "" {
		t.Fatalf("nil cipher DecryptStorageText() = %q, %v; want empty output and errStorageCipherUnavailable", got, err)
	}
	if got, err := EncryptStorageBytes(nilCipherCtx, "messages.attachment", "row", []byte("must not leak")); !errors.Is(err, errStorageCipherUnavailable) || len(got) != 0 {
		t.Fatalf("nil cipher EncryptStorageBytes() = %x, %v; want empty output and errStorageCipherUnavailable", got, err)
	}
	if got, err := DecryptStorageBytes(nilCipherCtx, "messages.attachment", "row", []byte("must not leak")); !errors.Is(err, errStorageCipherUnavailable) || len(got) != 0 {
		t.Fatalf("nil cipher DecryptStorageBytes() = %x, %v; want empty output and errStorageCipherUnavailable", got, err)
	}

	encryptedText, err := EncryptStorageText(cipherCtx, "settings.api_key", "row", "secret")
	if err != nil {
		t.Fatalf("explicit cipher EncryptStorageText() error: %v", err)
	}
	if !strings.HasPrefix(encryptedText, storageTextPrefix) {
		t.Fatalf("explicit cipher EncryptStorageText() = %q, want prefix %q", encryptedText, storageTextPrefix)
	}
	decryptedText, err := DecryptStorageText(cipherCtx, "settings.api_key", "row", encryptedText)
	if err != nil {
		t.Fatalf("explicit cipher DecryptStorageText() error: %v", err)
	}
	if decryptedText != "secret" {
		t.Fatalf("explicit cipher DecryptStorageText() = %q, want %q", decryptedText, "secret")
	}

	encryptedBytes, err := EncryptStorageBytes(cipherCtx, "messages.attachment", "row", []byte("secret"))
	if err != nil {
		t.Fatalf("explicit cipher EncryptStorageBytes() error: %v", err)
	}
	decryptedBytes, err := DecryptStorageBytes(cipherCtx, "messages.attachment", "row", encryptedBytes)
	if err != nil {
		t.Fatalf("explicit cipher DecryptStorageBytes() error: %v", err)
	}
	if string(decryptedBytes) != "secret" {
		t.Fatalf("explicit cipher DecryptStorageBytes() = %q, want %q", decryptedBytes, "secret")
	}
}
