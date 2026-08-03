// Command worker runs the Temporal worker that hosts CustomerRewardsWorkflow.
package main

import (
	"log"
	"os"

	"github.com/anthonywittig/rewards-poc/internal/rewards"
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	// The compose service sets this to temporal:7233; the default is the
	// host-published port, for running the worker directly with `go run`.
	address := env("TEMPORAL_HOSTPORT", "localhost:7233")
	namespace := env("TEMPORAL_NAMESPACE", "rewards")

	c, err := client.Dial(client.Options{
		HostPort:  address,
		Namespace: namespace,
	})
	if err != nil {
		log.Fatalf("unable to connect to Temporal at %s (namespace %q): %v\n"+
			"is the stack running? try `make up`", address, namespace, err)
	}
	defer c.Close()

	w := worker.New(c, rewards.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflows.CustomerRewardsWorkflow)

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
