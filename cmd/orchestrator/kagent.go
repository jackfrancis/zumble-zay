package main

// Activate the kagent substrate: blank-importing it runs its launcher.Register so
// LAUNCHER=kagent becomes selectable (docs/adr/0024). It lives in its own file
// (never a shared one) so it stays merge-clean with other concurrent substrate
// work, and carries no build tag — it pulls no third-party module — so it is
// always compiled in but inert unless selected.
import _ "github.com/jackfrancis/zumble-zay/internal/kagent"
