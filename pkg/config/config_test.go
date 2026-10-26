package config

import (
	"os"
	"testing"
	"time"
)

// clearAmbientEnv unsets every variable Load reads (restoring afterwards), so
// the surrounding environment cannot leak into assertions. godotenv also
// mutates the process environment, which the cleanups undo.
func clearAmbientEnv(t *testing.T) {
	t.Helper()
	vars := []string{
		"MQTT_BROKER_URL", "MQTT_CLIENT_ID", "MQTT_USERNAME", "MQTT_PASSWORD",
		"MQTT_TOPIC", "MQTT_AVAILABILITY_TOPIC", "MQTT_HUMAN_TOPIC", "MQTT_PAYLOAD_FORMAT", "HUMAN_COOLDOWN", "SNAPSHOT", "MQTT_DISCOVERY_PREFIX", "HOSTNAME_OR_IP",
		"DAHUA_USERNAME", "DAHUA_PASSWORD",
		"USERNAME", "PASSWORD", "HEALTH_PORT",
		"MQTT_USERNAME_FILE", "MQTT_PASSWORD_FILE", "DAHUA_USERNAME_FILE", "DAHUA_PASSWORD_FILE",
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
	if got := cfg.Mqtt.AvailabilityTopic; got != "doorbell/availability" {
		t.Errorf("AvailabilityTopic = %q, want default", got)
	}
	if got := cfg.Mqtt.HumanTopic; got != "doorbell/human" {
		t.Errorf("HumanTopic = %q, want default", got)
	}
	if got := cfg.Mqtt.PayloadFormat; got != PayloadEmpty {
		t.Errorf("PayloadFormat = %q, want default", got)
	}
	if got := cfg.Dahua.HumanCooldown; got != 10*time.Second {
		t.Errorf("HumanCooldown = %v, want default", got)
	}
	if got := cfg.HealthPort; got != "8080" {
		t.Errorf("HealthPort = %q, want default", got)
	}
}

// TestHumanTopicEmptyDisables locks in the opt-out: an explicitly empty
// MQTT_HUMAN_TOPIC must not be replaced by the default.
func TestHumanTopicEmptyDisables(t *testing.T) {
	clearAmbientEnv(t)
	t.Chdir(t.TempDir())
	setRequiredEnv(t)
	t.Setenv("DAHUA_USERNAME", "admin")
	t.Setenv("DAHUA_PASSWORD", "secret")
	t.Setenv("MQTT_HUMAN_TOPIC", "")

	cfg := Load()

	if got := cfg.Mqtt.HumanTopic; got != "" {
		t.Errorf("HumanTopic = %q, want empty to disable human detection", got)
	}
}

func TestSnapshotList(t *testing.T) {
	clearAmbientEnv(t)
	t.Chdir(t.TempDir())
	setRequiredEnv(t)
	t.Setenv("DAHUA_USERNAME", "admin")
	t.Setenv("DAHUA_PASSWORD", "secret")
	t.Setenv("SNAPSHOT", "press, human")

	cfg := Load()

	if !cfg.Mqtt.SnapshotEnabled(SnapshotPress) || !cfg.Mqtt.SnapshotEnabled(SnapshotHuman) {
		t.Errorf("Snapshot = %q, want press and human enabled (spaces trimmed)", cfg.Mqtt.Snapshot)
	}
	if got := cfg.Mqtt.HumanSnapshotTopic(); got != "doorbell/human/snapshot" {
		t.Errorf("HumanSnapshotTopic = %q", got)
	}
}

func TestSecretFiles(t *testing.T) {
	clearAmbientEnv(t)
	t.Chdir(t.TempDir())
	setRequiredEnv(t)
	os.Unsetenv("MQTT_PASSWORD")
	t.Setenv("DAHUA_USERNAME", "admin")
	if err := os.WriteFile("dahua_pw", []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("mqtt_pw", []byte("mqtt-from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAHUA_PASSWORD_FILE", "dahua_pw")
	t.Setenv("MQTT_PASSWORD_FILE", "mqtt_pw")
	t.Setenv("MQTT_USERNAME_FILE", "mqtt_pw") // MQTT_USERNAME is set directly: it wins

	cfg := Load()

	if got := cfg.Dahua.Password; got != "from-file" {
		t.Errorf("Dahua.Password = %q, want the file's value without its trailing newline", got)
	}
	if got := cfg.Mqtt.Password; got != "mqtt-from-file" {
		t.Errorf("Mqtt.Password = %q, want the file's value", got)
	}
	if got := cfg.Mqtt.Username; got != "mqtt-user" {
		t.Errorf("Mqtt.Username = %q, want the directly set variable to win over _FILE", got)
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
