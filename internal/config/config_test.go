package config

import (
	"encoding/base64"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/mint"
)

// A provisioned keypair (what `make mint-keys` / mint.GenerateKeyPair emits,
// base64 std-encoded) must be honored by loadMintKeys so a split deployment gets
// true issuer/verifier separation (docs/adr/0023): the orchestrator becomes an
// issuer from MINT_PRIVATE_KEY, and the web tier is verify-only from
// MINT_PUBLIC_KEY with no private key at all.
func TestLoadMintKeysHonorsProvisionedKeypair(t *testing.T) {
	priv, pub, err := mint.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	privB64 := base64.StdEncoding.EncodeToString(priv)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	const seed = "seed-should-be-ignored-when-keys-set"

	t.Run("orchestrator is an issuer", func(t *testing.T) {
		t.Setenv("MINT_PRIVATE_KEY", privB64)
		gotPriv, gotPub, err := loadMintKeys([]byte(seed))
		if err != nil {
			t.Fatalf("loadMintKeys: %v", err)
		}
		if base64.StdEncoding.EncodeToString(gotPriv) != privB64 {
			t.Error("MINT_PRIVATE_KEY was not honored")
		}
		if base64.StdEncoding.EncodeToString(gotPub) != pubB64 {
			t.Error("public key derived from the private key does not match the pair")
		}
	})

	t.Run("web tier is verify-only", func(t *testing.T) {
		t.Setenv("MINT_PRIVATE_KEY", "")
		t.Setenv("MINT_PUBLIC_KEY", pubB64)
		gotPriv, gotPub, err := loadMintKeys([]byte(seed))
		if err != nil {
			t.Fatalf("loadMintKeys: %v", err)
		}
		if gotPriv != nil {
			t.Error("a verify-only tier must not receive a private key")
		}
		if base64.StdEncoding.EncodeToString(gotPub) != pubB64 {
			t.Error("MINT_PUBLIC_KEY was not honored")
		}
	})

	t.Run("rejects a malformed key", func(t *testing.T) {
		t.Setenv("MINT_PRIVATE_KEY", "not-base64!!")
		if _, _, err := loadMintKeys([]byte(seed)); err == nil {
			t.Error("expected an error for a malformed MINT_PRIVATE_KEY")
		}
	})
}
