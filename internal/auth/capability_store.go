package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
)

// ErrCapabilityNotFound is returned by RevokeCapability when the cap is absent.
var ErrCapabilityNotFound = errors.New("capability not found")

// errCapabilityNoExpiry enforces that workflow capabilities always carry a TTL
// (part of the RedTeam mitigation: bounded credential lifetime).
var errCapabilityNoExpiry = errors.New("capability requires an ExpiresAt (nil forbidden)")

// GenerateCapability creates a new cap_ token for the given vault.
// ttl is required (pass a non-nil ExpiresAt); returns the raw token (shown once) and metadata.
func (s *Store) GenerateCapability(vault, label, mode, origin string, expiresAt *time.Time) (token string, cap Capability, err error) {
	if mode != ModeFull && mode != ModeObserve && mode != ModeWrite && mode != ModeAppend {
		err = fmt.Errorf("mode must be %q, %q, %q, or %q", ModeFull, ModeObserve, ModeWrite, ModeAppend)
		return
	}
	if expiresAt == nil {
		err = errCapabilityNoExpiry
		return
	}

	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		err = fmt.Errorf("generate random bytes: %w", err)
		return
	}
	token = "cap_" + base64.RawURLEncoding.EncodeToString(raw)

	h := sha256.Sum256(raw)
	storageHash := h[:16]
	capID := h[:8]

	cap = Capability{
		ID:          base64.RawURLEncoding.EncodeToString(capID),
		Vault:       vault,
		Label:       label,
		Mode:        mode,
		Origin:      origin,
		CreatedAt:   time.Now(),
		StorageHash: storageHash,
		ExpiresAt:   expiresAt,
	}

	data, marshalErr := json.Marshal(cap)
	if marshalErr != nil {
		err = fmt.Errorf("marshal capability: %w", marshalErr)
		return
	}

	batch := s.db.NewBatch()
	if setErr := batch.Set(capabilityStorageKey(storageHash), data, nil); setErr != nil {
		batch.Close()
		err = setErr
		return
	}
	if setErr := batch.Set(capabilityVaultIdxKey(vault, capID), storageHash, nil); setErr != nil {
		batch.Close()
		err = setErr
		return
	}
	err = batch.Commit(pebble.Sync)
	return
}

// ValidateCapability parses the cap_ token and returns its metadata.
func (s *Store) ValidateCapability(token string) (Capability, error) {
	const pfx = "cap_"
	if len(token) <= len(pfx) || token[:len(pfx)] != pfx {
		return Capability{}, fmt.Errorf("invalid token format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[len(pfx):])
	if err != nil || len(raw) != 32 {
		return Capability{}, fmt.Errorf("invalid token encoding")
	}
	h := sha256.Sum256(raw)
	data, closer, err := s.db.Get(capabilityStorageKey(h[:16]))
	if err != nil {
		return Capability{}, fmt.Errorf("invalid capability")
	}
	defer closer.Close()

	var cap Capability
	if err := json.Unmarshal(data, &cap); err != nil {
		return Capability{}, fmt.Errorf("corrupt capability record: %w", err)
	}
	if cap.ExpiresAt == nil || time.Now().After(*cap.ExpiresAt) {
		return Capability{}, fmt.Errorf("capability has expired")
	}
	return cap, nil
}

// ListCapabilities returns all capability metadata for a vault (tokens not included).
func (s *Store) ListCapabilities(vault string) ([]Capability, error) {
	prefix := capabilityVaultIdxPrefix(vault)
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	upper[len(upper)-1]++

	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return nil, fmt.Errorf("new iter: %w", err)
	}
	defer iter.Close()

	var caps []Capability
	for iter.First(); iter.Valid(); iter.Next() {
		storageHash := make([]byte, 16)
		copy(storageHash, iter.Value())
		data, closer, err := s.db.Get(capabilityStorageKey(storageHash))
		if err != nil {
			continue
		}
		var cap Capability
		if jsonErr := json.Unmarshal(data, &cap); jsonErr == nil {
			caps = append(caps, cap)
		}
		closer.Close()
	}
	return caps, iter.Error()
}

// RevokeCapability removes the capability with the given display ID from the vault.
func (s *Store) RevokeCapability(vault, capID string) error {
	idBytes, err := base64.RawURLEncoding.DecodeString(capID)
	if err != nil || len(idBytes) != 8 {
		return ErrCapabilityNotFound
	}
	idxKey := capabilityVaultIdxKey(vault, idBytes)
	storageHash, closer, err := s.db.Get(idxKey)
	if err != nil {
		return ErrCapabilityNotFound
	}
	hash := make([]byte, 16)
	copy(hash, storageHash)
	closer.Close()

	batch := s.db.NewBatch()
	if err := batch.Delete(capabilityStorageKey(hash), nil); err != nil {
		batch.Close()
		return err
	}
	if err := batch.Delete(idxKey, nil); err != nil {
		batch.Close()
		return err
	}
	return batch.Commit(pebble.Sync)
}
