package config

import (
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

type Dahua struct {
	Username string `envconfig:"USERNAME" required:"true"`
	Password string `envconfig:"PASSWORD" required:"true"`
	Host     string `envconfig:"HOSTNAME_OR_IP" required:"true"`
}

type Config struct {
	Mqtt  Mqtt
	Dahua Dahua
}

func Load() *Config {
	godotenv.Load(".env.local")
	godotenv.Load()

	var c Config
	err := envconfig.Process("", &c)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to process config")
	}

	return &c
}
