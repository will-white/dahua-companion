package config

import (
	"os"
	"testing"
)

// TestLoadPrecedence locks in the layering: real environment variables win
// over .env.local, which wins over .env (godotenv never overwrites a variable
// that is already set).
func TestLoadPrecedence(t *testing.T) {
	vars := []string{
		"MQTT_BROKER_URL", "MQTT_CLIENT_ID", "MQTT_USERNAME", "MQTT_PASSWORD",
		"MQTT_TOPIC", "HOSTNAME_OR_IP", "DAHUA_USERNAME", "DAHUA_PASSWORD",
		"HEALTH_PORT",
	}
	// godotenv mutates the process environment, so clear any ambient values
	// and restore them afterwards.
	for _, v := range vars {
		if old, ok := os.LookupEnv(v); ok {
			os.Unsetenv(v)
			t.Cleanup(func() { os.Setenv(v, old) })
		} else {
			t.Cleanup(func() { os.Unsetenv(v) })
		}
	}

	t.Chdir(t.TempDir())
	dotenv := `MQTT_BROKER_URL=tcp://from-env-file:1883
MQTT_CLIENT_ID=from-env-file
MQTT_USERNAME=from-env-file
MQTT_PASSWORD=from-env-file
HOSTNAME_OR_IP=from-env-file
DAHUA_USERNAME=from-env-file
DAHUA_PASSWORD=from-env-file
`
	if err := os.WriteFile(".env", []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".env.local", []byte("DAHUA_USERNAME=from-env-local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MQTT_CLIENT_ID", "from-real-env")

	cfg := Load()

	if got := cfg.Mqtt.Broker; got != "tcp://from-env-file:1883" {
		t.Errorf("Broker = %q, want value from .env", got)
	}
	if got := cfg.Dahua.Username; got != "from-env-local" {
		t.Errorf("Dahua.Username = %q, want .env.local to win over .env", got)
	}
	if got := cfg.Mqtt.ClientID; got != "from-real-env" {
		t.Errorf("ClientID = %q, want real environment to win over both", got)
	}
	if got := cfg.Mqtt.Topic; got != "doorbell/pressed" {
		t.Errorf("Topic = %q, want default", got)
	}
	if got := cfg.HealthPort; got != "8080" {
		t.Errorf("HealthPort = %q, want default", got)
	}
}
