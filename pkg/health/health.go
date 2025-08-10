package health

import (
	"context"
	"dahua_companion/pkg/dahua"
	"dahua_companion/pkg/mqtt"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/rs/zerolog/log"
)

type Server struct {
	server      *http.Server
	dahuaClient *dahua.Client
}

func New(dahuaClient *dahua.Client) *Server {
	server := &http.Server{Addr: ":8080"}
	s := &Server{server: server, dahuaClient: dahuaClient}
	http.HandleFunc("/health", s.healthCheckHandler())
	return s
}

func (s *Server) ListenAndServe() {
	go func() {
		log.Info().Msg("HTTP server started on 8080")
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msgf("Could not listen on :8080")
		}
	}()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) dahuaHealthCheck() string {
	doorbellStatus := "okay"
	url := fmt.Sprintf("http://%s/cgi-bin/configManager.cgi?action=setConfig&VSP_PaaS.Online=true", s.dahuaClient.Cfg.Host)
	res, err := s.dahuaClient.HttpClient.Get(url)
	if err != nil {
		log.Error().Err(err).Msg("client: error making http request")
		doorbellStatus = "HTTP Request Error"
	} else if res.StatusCode != http.StatusOK {
		log.Error().Int("status_code", res.StatusCode).Msgf("Expected 200 response got: %d", res.StatusCode)
		doorbellStatus = "HTTP Status Error " + res.Status
	}
	return doorbellStatus
}

func (s *Server) healthCheckHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		statusCode := http.StatusOK
		mqttStatus := "connected"
		if atomic.LoadInt32(&mqtt.IsConnected) != 1 {
			mqttStatus = "disconnected"
			statusCode = http.StatusServiceUnavailable
		}

		httpStatus := "connected"
		if atomic.LoadInt32(&dahua.IsConnected) != 1 {
			httpStatus = "disconnected"
			statusCode = http.StatusServiceUnavailable
		}

		doorbellStatus := s.dahuaHealthCheck()
		if doorbellStatus != "okay" {
			statusCode = http.StatusServiceUnavailable
		}

		if statusCode != http.StatusOK {
			status := fmt.Sprintf("MQTT: %s, HTTP: %s, Doorbell: %s", mqttStatus, httpStatus, doorbellStatus)
			w.Write([]byte(status))
		}

		w.WriteHeader(statusCode)
	}
}
