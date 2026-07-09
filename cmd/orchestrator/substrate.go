package main

// Activate the Agent Substrate substrate: blank-importing it runs its
// launcher.Register so LAUNCHER=substrate becomes selectable (docs/adr/0024,
// 0035). It lives in its own file (never a shared one) so it stays merge-clean
// with other concurrent substrate work, and carries no build tag — it pulls no
// third-party module (the actor-lifecycle gRPC is deliberately off the dispatch
// path) — so it is always compiled in but inert unless selected.
import _ "github.com/jackfrancis/zumble-zay/internal/substrate"
