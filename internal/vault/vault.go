// Package vault stores delegated provider credentials (OAuth tokens) obtained
// with a user's consent. ZZ is the durable holder; agent runtimes receive a
// vended credential only for the duration of a job (see docs/adr/0006). The
// default implementation keeps credentials in memory; a KMS-encrypted backend
// will implement the same interface.
package vault

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned when no credential exists for a user/provider.
var ErrNotFound = errors.New("vault: credential not found")

// Credential is a delegated provider credential held on a user's behalf.
type Credential struct {
	Provider     string    `json:"provider"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// Vault stores and retrieves delegated provider credentials, scoped by user.
type Vault interface {
	// Put stores (or replaces) the credential for a user and provider.
	Put(ctx context.Context, userID string, cred Credential) error
	// Get returns the credential for a user and provider, or ErrNotFound.
	Get(ctx context.Context, userID, provider string) (Credential, error)
	// Delete removes the credential for a user and provider. Deleting a
	// credential that does not exist is not an error, so logout can revoke
	// unconditionally.
	Delete(ctx context.Context, userID, provider string) error
}

// MemoryVault is an in-memory Vault for development and tests. The KMS-backed
// backend will implement the same interface.
type MemoryVault struct {
	mu    sync.RWMutex
	creds map[string]map[string]Credential // userID -> provider -> credential
}

// NewMemoryVault returns an empty in-memory vault.
func NewMemoryVault() *MemoryVault {
	return &MemoryVault{creds: make(map[string]map[string]Credential)}
}

// Put stores the credential for a user and provider, replacing any existing one.
func (v *MemoryVault) Put(_ context.Context, userID string, cred Credential) error {
	if userID == "" || cred.Provider == "" {
		return errors.New("vault: userID and provider are required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	byProvider := v.creds[userID]
	if byProvider == nil {
		byProvider = make(map[string]Credential)
		v.creds[userID] = byProvider
	}
	byProvider[cred.Provider] = cred
	return nil
}

// Get returns the stored credential for a user and provider, or ErrNotFound.
func (v *MemoryVault) Get(_ context.Context, userID, provider string) (Credential, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	cred, ok := v.creds[userID][provider]
	if !ok {
		return Credential{}, ErrNotFound
	}
	return cred, nil
}

// Delete removes the stored credential for a user and provider. It is a no-op
// (not an error) when none exists, so logout can call it unconditionally.
func (v *MemoryVault) Delete(_ context.Context, userID, provider string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if byProvider := v.creds[userID]; byProvider != nil {
		delete(byProvider, provider)
		if len(byProvider) == 0 {
			delete(v.creds, userID)
		}
	}
	return nil
}
