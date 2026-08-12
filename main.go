package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"dahua_companion/pkg/config"
	"dahua_companion/pkg/dahua"
	"dahua_companion/pkg/health"
	"dahua_companion/pkg/logger"
	"dahua_companion/pkg/mqtt"

	"github.com/rs/zerolog/log"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /health endpoint and exit; used by the Docker HEALTHCHECK")
	flag.Parse()
	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	logger.Init()
	cfg := config.Load()

	mqttClient := mqtt.New(&cfg.Mqtt)
	dahuaClient := dahua.New(&cfg.Dahua)
	// Availability mirrors the event stream: no stream, no doorbell.
	dahuaClient.OnConnectionChange = mqttClient.PublishAvailability
	healthServer := health.New(":"+cfg.HealthPort, mqttClient.IsConnected, dahuaClient.IsConnected, dahuaClient.Probe)
	healthServer.Start()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// The event itself is the signal; there is no payload.
		dahuaClient.Listen(ctx, func() { mqttClient.Publish("") })
	}()
	go func() {
		defer wg.Done()
		mqttClient.ProcessQueue(ctx)
	}()

	<-ctx.Done()
	log.Info().Msg("Shutting down...")
	// Restore default signal handling so a second signal kills immediately.
	stop()

	wg.Wait()
	mqttClient.Disconnect()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("Health server shutdown failed")
	}

	log.Info().Msg("Shutdown complete")
}

// runHealthcheck probes the local health endpoint. It backs the Docker
// HEALTHCHECK, since the scratch image has no shell or curl. It deliberately
// avoids config.Load: a healthcheck should not require full configuration.
func runHealthcheck() int {
	port := os.Getenv("HEALTH_PORT")
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/health", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health status %d\n", res.StatusCode)
		return 1
	}
	return 0
}
