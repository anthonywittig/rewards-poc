// Command worker runs the Temporal worker that hosts CustomerRewardsWorkflow.
//
// It reads the same TEMPORAL_* variables as the rest of the stack, so a plain
// `make worker` alongside `make up` is all that is needed. See README.
package main

import (
	"log"
	"os"

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

	log.Printf("worker polling task queue %q on %s (namespace %q), continue-as-new every %d adds",
		rewards.TaskQueue, address, namespace, rewards.EarnsPerRun)

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
