// Command worker runs the Temporal worker that hosts CustomerRewardsWorkflow.
//
// It reads the same TEMPORAL_* variables as the rest of the stack, so a plain
// `make worker` alongside `make up` is all that is needed. See README.
package main

import (
	"log"
	"os"
	"strconv"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	// Default to the host-published port rather than the compose-internal one:
	// the worker normally runs on the host via `make worker`, outside the
	// compose network, where "temporal:7233" does not resolve.
	address := env("TEMPORAL_HOSTPORT", "localhost:7233")
	namespace := env("TEMPORAL_NAMESPACE", "rewards")

	// Set before the worker starts polling -- see the hazard note on
	// rewards.SetEarnsPerRun. REWARDS_EARNS_PER_RUN=0 hands the decision to the
	// server instead, which is what production code should do.
	if v := os.Getenv("REWARDS_EARNS_PER_RUN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			log.Fatalf("REWARDS_EARNS_PER_RUN must be a non-negative integer, got %q", v)
		}
		rewards.SetEarnsPerRun(n)
	}

	c, err := client.Dial(client.Options{
		HostPort:  address,
		Namespace: namespace,
	})
	if err != nil {
		// The overwhelmingly likely cause during development is that the stack
		// is not up yet, so say so rather than surfacing a bare gRPC error.
		log.Fatalf("unable to connect to Temporal at %s (namespace %q): %v\n"+
			"is the stack running? try `make up`", address, namespace, err)
	}
	defer c.Close()

	w := worker.New(c, rewards.TaskQueue, worker.Options{})
	w.RegisterWorkflow(rewards.CustomerRewardsWorkflow)

	rollPolicy := "server-suggested"
	if n := rewards.EarnsPerRun(); n > 0 {
		rollPolicy = strconv.Itoa(n) + " adds per run"
	}
	log.Printf("worker polling task queue %q on %s (namespace %q), continue-as-new: %s",
		rewards.TaskQueue, address, namespace, rollPolicy)

	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker stopped: %v", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
