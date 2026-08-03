// Command api serves the HTTP surface over the rewards workflow.
package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/anthonywittig/rewards-poc/internal/httpapi"

	"go.temporal.io/sdk/client"
)

func main() {
	address := env("TEMPORAL_HOSTPORT", "localhost:7233")
	namespace := env("TEMPORAL_NAMESPACE", "rewards")
	// Browser-facing: this base URL ends up in response deep links, so it is
	// the published address, not the compose-internal one.
	temporalUI := env("TEMPORAL_UI_URL", "http://localhost:8080")
	addr := ":" + env("API_PORT", "8081")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	c, err := client.Dial(client.Options{
		HostPort:  address,
		Namespace: namespace,
		Logger:    logger,
	})
	if err != nil {
		log.Fatalf("unable to connect to Temporal at %s (namespace %q): %v\n"+
			"is the stack running? try `make up`", address, namespace, err)
	}
	defer c.Close()

	srv := &http.Server{
		Addr:         addr,
		Handler:      httpapi.New(c, logger, temporalUI, namespace).Routes(),
		ReadTimeout:  httpapi.DefaultTimeouts.Read,
		WriteTimeout: httpapi.DefaultTimeouts.Write,
		IdleTimeout:  httpapi.DefaultTimeouts.Idle,
	}

	logger.Info("api listening", "addr", addr, "temporal", address, "namespace", namespace)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("api stopped: %v", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
