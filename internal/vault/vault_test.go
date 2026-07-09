package vault

import (
	"context"
	"testing"
)

func TestMemoryVaultDelete(t *testing.T) {
	v := NewMemoryVault()
	ctx := context.Background()
	if err := v.Put(ctx, "u1", Credential{Provider: "github", AccessToken: "tok"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := v.Delete(ctx, "u1", "github"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := v.Get(ctx, "u1", "github"); err != ErrNotFound {
		t.Errorf("after Delete, Get err = %v, want ErrNotFound", err)
	}

	// Deleting a credential that no longer exists is a no-op, not an error, so
	// logout can revoke unconditionally.
	if err := v.Delete(ctx, "u1", "github"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
	if err := v.Delete(ctx, "does-not-exist", "github"); err != nil {
		t.Errorf("Delete of unknown user: %v", err)
	}
}
