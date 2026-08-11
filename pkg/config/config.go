package config

import (
	"os"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/rs/zerolog/log"
)

type Mqtt struct {
	Broker   string `envconfig:"MQTT_BROKER_URL" required:"true"`
	ClientID string `envconfig:"MQTT_CLIENT_ID" required:"true"`
	Username string `envconfig:"MQTT_USERNAME" required:"true"`
	Password string `envconfig:"MQTT_PASSWORD" required:"true"`
	Topic    string `envconfig:"MQTT_TOPIC" default:"doorbell/pressed"`
}

// The camera credentials are prefixed DAHUA_ on purpose: bare USERNAME and
// PASSWORD collide with variables the surrounding environment may already set
// (Windows always exports USERNAME), and real environment variables win over
// .env files. Load still honors the legacy bare names as a deprecated
// fallback, which is why these two are not marked required here.
type Dahua struct {
	Username string `envconfig:"DAHUA_USERNAME"`
	Password string `envconfig:"DAHUA_PASSWORD"`
	Host     string `envconfig:"HOSTNAME_OR_IP" required:"true"`
}

type Config struct {
	Mqtt       Mqtt
	Dahua      Dahua
	HealthPort string `envconfig:"HEALTH_PORT" default:"8080"`
}

func Load() *Config {
	// The .env files are optional local overrides; a missing file is fine.
	_ = godotenv.Load(".env.local")
	_ = godotenv.Load()

	var c Config
	err := envconfig.Process("", &c)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to process config")
	}

	// Deprecated fallback for deployments configured before the DAHUA_ prefix.
	if c.Dahua.Username == "" {
		if legacy := os.Getenv("USERNAME"); legacy != "" {
			log.Warn().Msg("USERNAME is deprecated and will be removed in a future release; set DAHUA_USERNAME")
			c.Dahua.Username = legacy
		}
	}
	if c.Dahua.Password == "" {
		if legacy := os.Getenv("PASSWORD"); legacy != "" {
			log.Warn().Msg("PASSWORD is deprecated and will be removed in a future release; set DAHUA_PASSWORD")
			c.Dahua.Password = legacy
		}
	}
	if c.Dahua.Username == "" || c.Dahua.Password == "" {
		log.Fatal().Msg("DAHUA_USERNAME and DAHUA_PASSWORD are required")
	}

	return &c
}
