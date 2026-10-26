package health

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// probeTimeout bounds the live camera check; the shared Dahua HTTP client has
// no client-wide timeout because of the long-poll event stream.
const probeTimeout = 5 * time.Second

// probeCacheTTL is how long a successful camera probe is trusted. The event
// stream's own watchdog notices a dead camera within seconds, so re-probing on
// every request (Docker alone checks every 30s, monitors often more) only adds
// authenticated load on the camera. Failures are never cached, so recovery
// shows up on the next request.
const probeCacheTTL = 60 * time.Second

type Server struct {
	server         *http.Server
	mqttConnected  func() bool
	dahuaConnected func() bool
	probe          func(context.Context) error

	// probeMu serializes probes, so concurrent health requests share one
	// camera round trip, and guards lastProbeOK.
	probeMu     sync.Mutex
	lastProbeOK time.Time
	probeTTL    time.Duration
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
		probeTTL:       probeCacheTTL,
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

// probeCamera runs the live camera probe unless one succeeded within
// probeTTL.
func (s *Server) probeCamera(ctx context.Context) error {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if !s.lastProbeOK.IsZero() && time.Since(s.lastProbeOK) < s.probeTTL {
		return nil
	}
	if err := s.probe(ctx); err != nil {
		s.lastProbeOK = time.Time{}
		return err
	}
	s.lastProbeOK = time.Now()
	return nil
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
	if err := s.probeCamera(ctx); err != nil {
		log.Error().Err(err).Msg("Camera probe failed")
		doorbellStatus = "unreachable"
		statusCode = http.StatusServiceUnavailable
	}

	w.WriteHeader(statusCode)
	fmt.Fprintf(w, "MQTT: %s, HTTP: %s, Doorbell: %s", mqttStatus, streamStatus, doorbellStatus)
}
