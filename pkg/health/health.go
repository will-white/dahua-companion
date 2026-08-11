package health

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

// probeTimeout bounds the live camera check; the shared Dahua HTTP client has
// no client-wide timeout because of the long-poll event stream.
const probeTimeout = 5 * time.Second

type Server struct {
	server         *http.Server
	mqttConnected  func() bool
	dahuaConnected func() bool
	probe          func(context.Context) error
}

// New builds the health server. mqttConnected and dahuaConnected report the
// broker and event stream connection states; probe performs a live read-only
// check against the camera.
func New(addr string, mqttConnected, dahuaConnected func() bool, probe func(context.Context) error) *Server {
	mux := http.NewServeMux()
	s := &Server{
		server: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
		mqttConnected:  mqttConnected,
		dahuaConnected: dahuaConnected,
		probe:          probe,
	}
	mux.HandleFunc("/health", s.healthCheckHandler)
	return s
}

// Start serves in a background goroutine.
func (s *Server) Start() {
	go func() {
		log.Info().Str("addr", s.server.Addr).Msg("Health server started")
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("Health server failed")
		}
	}()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	statusCode := http.StatusOK

	mqttStatus := "connected"
	if !s.mqttConnected() {
		mqttStatus = "disconnected"
		statusCode = http.StatusServiceUnavailable
	}

	streamStatus := "connected"
	if !s.dahuaConnected() {
		streamStatus = "disconnected"
		statusCode = http.StatusServiceUnavailable
	}

	doorbellStatus := "okay"
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	if err := s.probe(ctx); err != nil {
		log.Error().Err(err).Msg("Camera probe failed")
		doorbellStatus = "unreachable"
		statusCode = http.StatusServiceUnavailable
	}

	w.WriteHeader(statusCode)
	fmt.Fprintf(w, "MQTT: %s, HTTP: %s, Doorbell: %s", mqttStatus, streamStatus, doorbellStatus)
}
