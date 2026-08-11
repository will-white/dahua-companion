package config

import (
	"os"
	"testing"
)

// clearAmbientEnv unsets every variable Load reads (restoring afterwards), so
// the surrounding environment cannot leak into assertions. godotenv also
// mutates the process environment, which the cleanups undo.
func clearAmbientEnv(t *testing.T) {
	t.Helper()
	vars := []string{
		"MQTT_BROKER_URL", "MQTT_CLIENT_ID", "MQTT_USERNAME", "MQTT_PASSWORD",
		"MQTT_TOPIC", "HOSTNAME_OR_IP", "DAHUA_USERNAME", "DAHUA_PASSWORD",
		"USERNAME", "PASSWORD", "HEALTH_PORT",
	}
	for _, v := range vars {
		if old, ok := os.LookupEnv(v); ok {
			os.Unsetenv(v)
			t.Cleanup(func() { os.Setenv(v, old) })
		} else {
			t.Cleanup(func() { os.Unsetenv(v) })
		}
	}
}

// setRequiredEnv sets everything Load requires except the camera credentials,
// which the credential tests vary.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MQTT_BROKER_URL", "tcp://broker:1883")
	t.Setenv("MQTT_CLIENT_ID", "test-client")
	t.Setenv("MQTT_USERNAME", "mqtt-user")
	t.Setenv("MQTT_PASSWORD", "mqtt-pass")
	t.Setenv("HOSTNAME_OR_IP", "doorbell")
}

// TestLoadPrecedence locks in the layering: real environment variables win
// over .env.local, which wins over .env (godotenv never overwrites a variable
// that is already set).
func TestLoadPrecedence(t *testing.T) {
	clearAmbientEnv(t)
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

func TestLegacyCredentialFallback(t *testing.T) {
	clearAmbientEnv(t)
	t.Chdir(t.TempDir())
	setRequiredEnv(t)
	t.Setenv("USERNAME", "legacy-user")
	t.Setenv("PASSWORD", "legacy-pass")

	cfg := Load()

	if got := cfg.Dahua.Username; got != "legacy-user" {
		t.Errorf("Dahua.Username = %q, want legacy USERNAME fallback", got)
	}
	if got := cfg.Dahua.Password; got != "legacy-pass" {
		t.Errorf("Dahua.Password = %q, want legacy PASSWORD fallback", got)
	}
}

func TestNewCredentialNamesWinOverLegacy(t *testing.T) {
	clearAmbientEnv(t)
	t.Chdir(t.TempDir())
	setRequiredEnv(t)
	t.Setenv("USERNAME", "legacy-user")
	t.Setenv("PASSWORD", "legacy-pass")
	t.Setenv("DAHUA_USERNAME", "new-user")
	t.Setenv("DAHUA_PASSWORD", "new-pass")

	cfg := Load()

	if got := cfg.Dahua.Username; got != "new-user" {
		t.Errorf("Dahua.Username = %q, want DAHUA_USERNAME to win over legacy", got)
	}
	if got := cfg.Dahua.Password; got != "new-pass" {
		t.Errorf("Dahua.Password = %q, want DAHUA_PASSWORD to win over legacy", got)
	}
}
