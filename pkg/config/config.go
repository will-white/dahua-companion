package config

import (
	"os"
	"slices"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/rs/zerolog/log"
)

// Event payload formats for MQTT_PAYLOAD_FORMAT.
const (
	PayloadEmpty = "empty"
	PayloadJSON  = "json"
)

// Snapshot sources for SNAPSHOT_SOURCE.
const (
	SnapshotFromRequest = "request"
	SnapshotFromEvent   = "event"
)

// Camera API schemes for DAHUA_SCHEME.
const (
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// Events that SNAPSHOT can name.
const (
	SnapshotPress = "press"
	SnapshotHuman = "human"
)

type Mqtt struct {
	Broker            string `envconfig:"MQTT_BROKER_URL" required:"true"`
	ClientID          string `envconfig:"MQTT_CLIENT_ID" required:"true"`
	Username          string `envconfig:"MQTT_USERNAME" required:"true"`
	Password          string `envconfig:"MQTT_PASSWORD" required:"true"`
	Topic             string `envconfig:"MQTT_TOPIC" default:"doorbell/pressed"`
	AvailabilityTopic string `envconfig:"MQTT_AVAILABILITY_TOPIC" default:"doorbell/availability"`
	// HumanTopic receives human detection events. Setting it explicitly empty
	// disables human detection (envconfig only applies the default when the
	// variable is unset).
	HumanTopic string `envconfig:"MQTT_HUMAN_TOPIC" default:"doorbell/human"`
	// PayloadFormat is PayloadEmpty (the event is the signal) or PayloadJSON
	// (event type, timestamp and camera channel).
	PayloadFormat string `envconfig:"MQTT_PAYLOAD_FORMAT" default:"empty"`
	// Snapshot lists the events (SnapshotPress, SnapshotHuman) that also
	// publish a camera still to "<event topic>/snapshot"; empty disables.
	Snapshot []string `envconfig:"SNAPSHOT"`
	// SnapshotSource is SnapshotFromRequest (fetch snapshot.cgi right after
	// the event; works on any camera) or SnapshotFromEvent (the camera's
	// event-snapshot stream, captured at the moment of the event).
	SnapshotSource string `envconfig:"SNAPSHOT_SOURCE" default:"request"`
	// DiscoveryPrefix enables Home Assistant MQTT discovery when set (HA's
	// default prefix is "homeassistant"); empty disables it.
	DiscoveryPrefix string `envconfig:"MQTT_DISCOVERY_PREFIX"`
}

// SnapshotEnabled reports whether SNAPSHOT names event.
func (m *Mqtt) SnapshotEnabled(event string) bool {
	return slices.Contains(m.Snapshot, event)
}

// PressSnapshotTopic is where press snapshots are published.
func (m *Mqtt) PressSnapshotTopic() string { return m.Topic + "/snapshot" }

// HumanSnapshotTopic is where human detection snapshots are published.
func (m *Mqtt) HumanSnapshotTopic() string { return m.HumanTopic + "/snapshot" }

// The camera credentials are prefixed DAHUA_ on purpose: bare USERNAME and
// PASSWORD collide with variables the surrounding environment may already set
// (Windows always exports USERNAME), and real environment variables win over
// .env files. Load still honors the legacy bare names as a deprecated
// fallback, which is why these two are not marked required here.
type Dahua struct {
	Username string `envconfig:"DAHUA_USERNAME"`
	Password string `envconfig:"DAHUA_PASSWORD"`
	Host     string `envconfig:"HOSTNAME_OR_IP" required:"true"`
	// Scheme is SchemeHTTP (default) or SchemeHTTPS for the camera API.
	Scheme string `envconfig:"DAHUA_SCHEME" default:"http"`
	// CAFile, with https, is a PEM bundle to verify the camera against
	// instead of the system roots - typically its self-signed certificate.
	CAFile string `envconfig:"DAHUA_CA_FILE"`
	// TLSInsecure, with https, skips certificate verification entirely.
	TLSInsecure bool `envconfig:"DAHUA_TLS_INSECURE"`
	// HumanCooldown suppresses human detections that follow a published one
	// within this window; 0 publishes every detection.
	HumanCooldown time.Duration `envconfig:"HUMAN_COOLDOWN" default:"10s"`
}

type Config struct {
	Mqtt       Mqtt
	Dahua      Dahua
	HealthPort string `envconfig:"HEALTH_PORT" default:"8080"`
}

// secretVars may instead be supplied as <NAME>_FILE, a path to a file holding
// the value (Docker/compose secrets mount them under /run/secrets).
var secretVars = []string{"MQTT_USERNAME", "MQTT_PASSWORD", "DAHUA_USERNAME", "DAHUA_PASSWORD"}

// loadSecretFiles sets each secret variable from its _FILE counterpart. A
// directly set variable wins, matching the "real environment wins" layering;
// setting both is reported since one of them is being ignored.
func loadSecretFiles() {
	for _, name := range secretVars {
		path := os.Getenv(name + "_FILE")
		if path == "" {
			continue
		}
		if _, set := os.LookupEnv(name); set {
			log.Warn().Str("var", name).Msg("Both " + name + " and " + name + "_FILE are set; using " + name)
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			log.Fatal().Err(err).Str("var", name+"_FILE").Msg("Failed to read secret file")
		}
		// Editors and `echo` add a trailing newline that is never part of
		// the secret.
		if err := os.Setenv(name, strings.TrimRight(string(b), "\r\n")); err != nil {
			log.Fatal().Err(err).Str("var", name).Msg("Failed to set secret from file")
		}
	}
}

func Load() *Config {
	// The .env files are optional local overrides; a missing file is fine.
	_ = godotenv.Load(".env.local")
	_ = godotenv.Load()

	loadSecretFiles()

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
	if c.Mqtt.PayloadFormat != PayloadEmpty && c.Mqtt.PayloadFormat != PayloadJSON {
		log.Fatal().Str("format", c.Mqtt.PayloadFormat).Msg("MQTT_PAYLOAD_FORMAT must be empty or json")
	}
	if c.Mqtt.SnapshotSource != SnapshotFromRequest && c.Mqtt.SnapshotSource != SnapshotFromEvent {
		log.Fatal().Str("source", c.Mqtt.SnapshotSource).Msg("SNAPSHOT_SOURCE must be request or event")
	}
	if c.Dahua.Scheme != SchemeHTTP && c.Dahua.Scheme != SchemeHTTPS {
		log.Fatal().Str("scheme", c.Dahua.Scheme).Msg("DAHUA_SCHEME must be http or https")
	}
	if c.Dahua.Scheme == SchemeHTTP && (c.Dahua.CAFile != "" || c.Dahua.TLSInsecure) {
		log.Warn().Msg("DAHUA_CA_FILE / DAHUA_TLS_INSECURE have no effect unless DAHUA_SCHEME=https")
	}
	if c.Dahua.TLSInsecure && c.Dahua.CAFile != "" {
		log.Warn().Msg("DAHUA_TLS_INSECURE skips verification, so DAHUA_CA_FILE is ignored")
	}
	for i, event := range c.Mqtt.Snapshot {
		event = strings.TrimSpace(event)
		c.Mqtt.Snapshot[i] = event
		switch {
		case event != SnapshotPress && event != SnapshotHuman:
			log.Fatal().Str("event", event).Msg("SNAPSHOT entries must be press or human")
		case event == SnapshotHuman && c.Mqtt.HumanTopic == "":
			log.Fatal().Msg("SNAPSHOT includes human but human detection is disabled (MQTT_HUMAN_TOPIC is empty)")
		}
	}
	if c.Dahua.Username == "" || c.Dahua.Password == "" {
		log.Fatal().Msg("DAHUA_USERNAME and DAHUA_PASSWORD are required")
	}

	return &c
}
