package kritui_db

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
)

const (
	// StorageKeySize is the required AES-256 key length in bytes.
	StorageKeySize = 32

	storageTextPrefix = "enc:v1:"
	storageMagic      = "KRST"
	storageVersion    = byte(1)
	storageNonceSize  = 12
	storageTagSize    = 16
	storageHeaderSize = len(storageMagic) + 1 + storageNonceSize
)

var (
	// ErrStorageDecryption reports an envelope that is malformed, belongs to a
	// different scope/owner/row, uses an unknown format version, or fails
	// authentication. It deliberately carries no input, key, or ciphertext.
	ErrStorageDecryption = errors.New("storage decryption failed")

	errStorageCipherUnavailable = errors.New("storage cipher unavailable")

	// storageEncryptionRequired is a process-wide, one-way guard set only by
	// RequireStorageEncryption. It prevents a lost request or worker context
	// from silently writing or reading plaintext in the real application.
	storageEncryptionRequired atomic.Bool
)

// RequireStorageEncryption permanently requires a StorageCipher on every
// storage helper call in this process. Production Run must call it exactly once
// after validating the mandatory key and before serving requests or starting
// workers. The guard is one-way by design: there is no disable, reset, or
// exported toggle, and no environment bypass. After it is set, missing or nil
// context ciphers return errStorageCipherUnavailable instead of falling back to
// plaintext. Tests and other trusted direct callers that never set it retain
// the legacy plaintext behavior.
func RequireStorageEncryption() {
	storageEncryptionRequired.Store(true)
}

// contextStorageCipher returns the cipher attached to ctx, or nil when legacy
// plaintext behavior is still allowed. Once RequireStorageEncryption has been
// called, a missing or nil cipher is an error rather than a plaintext fallback.
func contextStorageCipher(ctx context.Context) (*StorageCipher, error) {
	storage := StorageCipherFromContext(ctx)
	if storage != nil {
		return storage, nil
	}
	if storageEncryptionRequired.Load() {
		return nil, errStorageCipherUnavailable
	}
	return nil, nil
}

type storageCipherContextKey struct{}

// StorageCipher encrypts at-rest values with AES-256-GCM. The zero value and a
// nil pointer are invalid and fail safely; obtain a usable value only from
// NewStorageCipher.
type StorageCipher struct {
	aead cipher.AEAD
}

// NewStorageCipher builds a StorageCipher from a 32-byte AES-256 key. The key
// is copied by aes.NewCipher, so callers may reuse or clear their slice.
func NewStorageCipher(key []byte) (*StorageCipher, error) {
	if len(key) != StorageKeySize {
		return nil, fmt.Errorf("storage cipher key must be %d bytes", StorageKeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("storage cipher initialization failed")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("storage cipher initialization failed")
	}
	return &StorageCipher{aead: aead}, nil
}

// WithStorageCipher attaches storage encryption to ctx. Production request,
// startup, and worker contexts must always carry a cipher.
func WithStorageCipher(ctx context.Context, storage *StorageCipher) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, storageCipherContextKey{}, storage)
}

// StorageCipherFromContext returns the cipher attached by WithStorageCipher,
// or nil when the context carries none.
func StorageCipherFromContext(ctx context.Context) *StorageCipher {
	if ctx == nil {
		return nil
	}
	storage, _ := ctx.Value(storageCipherContextKey{}).(*StorageCipher)
	return storage
}

// Seal encrypts plaintext into a self-describing binary envelope: magic and
// format version, a fresh random nonce, then the GCM ciphertext and tag. Scope,
// ownerID, and rowID are authenticated as additional data, so an envelope can
// only be opened with the same binding.
func (s *StorageCipher) Seal(plaintext []byte, scope string, ownerID int64, rowID string) ([]byte, error) {
	if s == nil || s.aead == nil {
		return nil, errStorageCipherUnavailable
	}
	envelope := make([]byte, storageHeaderSize, storageHeaderSize+len(plaintext)+s.aead.Overhead())
	copy(envelope, storageMagic)
	envelope[len(storageMagic)] = storageVersion
	nonce := envelope[len(storageMagic)+1 : storageHeaderSize]
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, errors.New("storage encryption failed")
	}
	return s.aead.Seal(envelope, nonce, plaintext, storageAAD(scope, ownerID, rowID)), nil
}

// Open authenticates and decrypts a binary envelope produced by Seal. Every
// malformed, truncated, unknown-version, mismatched, or tampered envelope
// returns ErrStorageDecryption and never the raw input.
func (s *StorageCipher) Open(envelope []byte, scope string, ownerID int64, rowID string) ([]byte, error) {
	if s == nil || s.aead == nil {
		return nil, errStorageCipherUnavailable
	}
	if len(envelope) < storageHeaderSize+s.aead.Overhead() {
		return nil, ErrStorageDecryption
	}
	if string(envelope[:len(storageMagic)]) != storageMagic {
		return nil, ErrStorageDecryption
	}
	if envelope[len(storageMagic)] != storageVersion {
		return nil, ErrStorageDecryption
	}
	nonce := envelope[len(storageMagic)+1 : storageHeaderSize]
	plaintext, err := s.aead.Open(nil, nonce, envelope[storageHeaderSize:], storageAAD(scope, ownerID, rowID))
	if err != nil {
		return nil, ErrStorageDecryption
	}
	return plaintext, nil
}

// storageAAD builds an unambiguous binding of the envelope format, caller
// scope, owner, and row. Variable-length fields carry explicit lengths and the
// owner is a fixed-width big-endian value.
func storageAAD(scope string, ownerID int64, rowID string) []byte {
	aad := make([]byte, 0, len(storageMagic)+1+8+8+len(scope)+8+len(rowID))
	aad = append(aad, storageMagic...)
	aad = append(aad, storageVersion)
	aad = appendStorageAADField(aad, scope)
	var owner [8]byte
	binary.BigEndian.PutUint64(owner[:], uint64(ownerID))
	aad = append(aad, owner[:]...)
	return appendStorageAADField(aad, rowID)
}

func appendStorageAADField(aad []byte, value string) []byte {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	aad = append(aad, length[:]...)
	return append(aad, value...)
}

// EncryptStorageText encrypts value for the context user and returns
// "enc:v1:" followed by standard base64 of the binary envelope.
//
// Compatibility mode: before RequireStorageEncryption is called, a context
// with no StorageCipher returns the value unchanged, preserving trusted direct
// database callers and existing raw SQL tests. Production Run sets the one-way
// guard after validating the key and installs a cipher in every request,
// startup, and worker context; after that, a missing or nil cipher fails with
// errStorageCipherUnavailable and never writes plaintext. With a cipher
// present, decryption rejects plaintext, including the empty string; callers
// alone may interpret a SQL blank sentinel as absent.
func EncryptStorageText(ctx context.Context, scope, rowID, value string) (string, error) {
	storage, err := contextStorageCipher(ctx)
	if err != nil {
		return "", err
	}
	if storage == nil {
		return value, nil
	}
	envelope, err := storage.Seal([]byte(value), scope, UserID(ctx), rowID)
	if err != nil {
		return "", err
	}
	return storageTextPrefix + base64.StdEncoding.EncodeToString(envelope), nil
}

// DecryptStorageText reverses EncryptStorageText for the context user. Before
// RequireStorageEncryption is called, a context with no cipher returns value
// unchanged for legacy direct callers. After the guard is set, a missing or nil
// cipher fails with errStorageCipherUnavailable; with a cipher it requires the
// text envelope and rejects plaintext.
func DecryptStorageText(ctx context.Context, scope, rowID, value string) (string, error) {
	storage, err := contextStorageCipher(ctx)
	if err != nil {
		return "", err
	}
	if storage == nil {
		return value, nil
	}
	if !strings.HasPrefix(value, storageTextPrefix) {
		return "", ErrStorageDecryption
	}
	envelope, err := base64.StdEncoding.Strict().DecodeString(value[len(storageTextPrefix):])
	if err != nil {
		return "", ErrStorageDecryption
	}
	plaintext, err := storage.Open(envelope, scope, UserID(ctx), rowID)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// EncryptStorageBytes encrypts value for the context user into a binary
// envelope. Before RequireStorageEncryption is called, a context with no cipher
// returns value unchanged for legacy direct callers. After the guard is set, a
// missing or nil cipher fails with errStorageCipherUnavailable.
func EncryptStorageBytes(ctx context.Context, scope, rowID string, value []byte) ([]byte, error) {
	storage, err := contextStorageCipher(ctx)
	if err != nil {
		return nil, err
	}
	if storage == nil {
		return value, nil
	}
	return storage.Seal(value, scope, UserID(ctx), rowID)
}

// DecryptStorageBytes reverses EncryptStorageBytes for the context user.
// Before RequireStorageEncryption is called, a context with no cipher returns
// value unchanged for legacy direct callers. After the guard is set, a missing
// or nil cipher fails with errStorageCipherUnavailable; with a cipher it
// rejects plaintext, including an empty slice.
func DecryptStorageBytes(ctx context.Context, scope, rowID string, value []byte) ([]byte, error) {
	storage, err := contextStorageCipher(ctx)
	if err != nil {
		return nil, err
	}
	if storage == nil {
		return value, nil
	}
	return storage.Open(value, scope, UserID(ctx), rowID)
}
