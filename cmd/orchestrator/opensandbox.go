package main

// Activate the OpenSandbox substrate: blank-importing it runs its
// launcher.Register so LAUNCHER=opensandbox becomes selectable (docs/adr/0024,
// 0027). It lives in its own file (never a shared one) so adding it stays
// merge-clean with other concurrent substrate work. Unlike agent-sandbox it
// carries no build tag — it pulls no third-party module — so it is always
// compiled in but inert unless selected.
import _ "github.com/jackfrancis/zumble-zay/internal/opensandbox"
