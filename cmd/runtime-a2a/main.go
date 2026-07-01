// Command runtime-a2a serves the ZZ agent runtime over the A2A protocol
// (docs/adr/0024). It is the durable-service counterpart to cmd/runtime: instead
// of reading one job from the environment and exiting, it stays up and executes
// each job dispatched to it as an A2A message/send, with the per-job parameters
// carried in the task metadata. It runs the same agent.Run, so behaviour is
// identical to the pod runtime; only the invocation path differs.
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/jackfrancis/zumble-zay/internal/agenta2a"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	srv := agenta2a.New(agenta2a.WithLogger(log))

	log.Info("zz runtime-a2a server starting", "addr", addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}
