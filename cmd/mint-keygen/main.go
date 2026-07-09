// Command mint-keygen prints a fresh Ed25519 job-token keypair (docs/adr/0023)
// as the two environment values a split deployment expects: the orchestrator's
// MINT_PRIVATE_KEY (a secret, the sole signer) and the web tier's MINT_PUBLIC_KEY
// (config, verify-only). Provisioning an independent pair — rather than letting
// both tiers derive one from SESSION_SECRET — gives true issuer/verifier
// separation, so only the orchestrator can mint a job token.
package main

import (
	"encoding/base64"
	"fmt"
	"os"

	"github.com/jackfrancis/zumble-zay/internal/mint"
)

func main() {
	priv, pub, err := mint.GenerateKeyPair()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mint-keygen: generate keypair:", err)
		os.Exit(1)
	}
	fmt.Println("# Job-token signing keypair (docs/adr/0023) — provision each half to ONE tier.")
	fmt.Println("# Orchestrator only (secret; the sole signer):")
	fmt.Printf("MINT_PRIVATE_KEY=%s\n", base64.StdEncoding.EncodeToString(priv))
	fmt.Println("# Web tier only (config, not a secret; verify-only):")
	fmt.Printf("MINT_PUBLIC_KEY=%s\n", base64.StdEncoding.EncodeToString(pub))
}
