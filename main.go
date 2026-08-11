package main

import (
	"context"
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
	logger.Init()
	cfg := config.Load()

	mqttClient := mqtt.New(&cfg.Mqtt)
	dahuaClient := dahua.New(&cfg.Dahua)
	healthServer := health.New(":8080", mqttClient.IsConnected, dahuaClient.IsConnected, dahuaClient.Probe)
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
