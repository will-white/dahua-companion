package main

import (
	"context"
	"os"
	"os/signal"
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
	healthServer := health.New(dahuaClient)

	healthServer.ListenAndServe()

	// Channel to signal shutdown
	shutdown := make(chan struct{})
	messageChan := make(chan string)

	go dahuaClient.Listen(shutdown, messageChan)
	go mqttClient.ProcessQueue(shutdown)

	go func() {
		for {
			select {
			case <-shutdown:
				return
			case msg := <-messageChan:
				mqttClient.Publish(msg)
			}
		}
	}()

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Info().Msg("Shutting down...")

	// Signal the shutdown of the HTTP stream loop
	close(shutdown)

	// Close MQTT connection
	mqttClient.Disconnect()

	// Close HTTP server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := healthServer.Shutdown(ctx); err != nil {
		log.Fatal().Err(err).Msg("HTTP server Shutdown")
	}

	log.Info().Msg("Shutdown complete")
}
