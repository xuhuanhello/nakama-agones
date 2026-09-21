package fleetmanager

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xuhuanhello/nakama-agones/internal/agones"
)

const (
	maxServerEnvironmentBytes      = 32 << 10
	maxServerEnvironmentEntries    = 64
	maxServerEnvironmentValueBytes = 8 << 10
)

type Config struct {
	DeploymentID, DatabaseURL, ControlURL string
	BuildHash, Region, PortName           string
	MaxRooms, MinInstances, MaxInstances  int
	Kubernetes                            agones.Config
	AdminToken                            string
	SigningKey                            []byte
	// Values are supplied only to the game server, never stored in fleet state.
	ServerEnvironment map[string]string `json:"-"`
	AllowHTTP         bool
	Mode              string
	Interval          time.Duration

	LaunchTimeout, AllocationTimeout, PrepareTimeout int64
	ReservationTTL, ReconnectSeconds                 int64
	HeartbeatTimeout, LostTimeout                    int64
	IdleSeconds, ProviderPollSeconds                 int64
	MinLifetimeForAdmission                          int64
	MaxQueueAgeSeconds                               float64
}

func FromEnv() (Config, error) {
	c := Config{DeploymentID: env("AGONES_FLEET_DEPLOYMENT_ID", "default"), DatabaseURL: os.Getenv("AGONES_FLEET_DATABASE_URL"), ControlURL: os.Getenv("AGONES_FLEET_CONTROL_URL"), BuildHash: os.Getenv("AGONES_FLEET_BUILD_HASH"), Region: os.Getenv("AGONES_FLEET_REGION"), PortName: env("AGONES_FLEET_PORT_NAME", "game"), AdminToken: os.Getenv("AGONES_FLEET_ADMIN_TOKEN"), AllowHTTP: os.Getenv("AGONES_FLEET_ALLOW_HTTP") == "true", Mode: env("AGONES_FLEET_MODE", "production"), Interval: time.Second, LaunchTimeout: 120, AllocationTimeout: 180, PrepareTimeout: 20, ReservationTTL: 60, ReconnectSeconds: 90, HeartbeatTimeout: 8, LostTimeout: 60, IdleSeconds: 600, ProviderPollSeconds: 10, MaxQueueAgeSeconds: 2}
	c.MinLifetimeForAdmission = 1800
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("AGONES_FLEET_DATABASE_URL is required")
	}
	for key, dest := range map[string]*int{"AGONES_FLEET_MAX_ROOMS": &c.MaxRooms, "AGONES_FLEET_MIN_INSTANCES": &c.MinInstances, "AGONES_FLEET_MAX_INSTANCES": &c.MaxInstances} {
		value := os.Getenv(key)
		if value == "" && key == "AGONES_FLEET_MIN_INSTANCES" {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return c, fmt.Errorf("%s must be an integer", key)
		}
		*dest = n
	}
	for key, dest := range map[string]*int64{"AGONES_FLEET_IDLE_SECONDS": &c.IdleSeconds, "AGONES_FLEET_LAUNCH_TIMEOUT": &c.LaunchTimeout, "AGONES_FLEET_ALLOCATION_TIMEOUT": &c.AllocationTimeout, "AGONES_FLEET_PROVIDER_POLL_SECONDS": &c.ProviderPollSeconds, "AGONES_FLEET_MIN_LIFETIME_FOR_ADMISSION_SECONDS": &c.MinLifetimeForAdmission} {
		if v := os.Getenv(key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return c, fmt.Errorf("invalid %s", key)
			}
			*dest = n
		}
	}
	key, err := base64.RawURLEncoding.DecodeString(os.Getenv("AGONES_FLEET_SIGNING_KEY"))
	if err != nil {
		return c, fmt.Errorf("AGONES_FLEET_SIGNING_KEY must be base64url")
	}
	c.SigningKey = key
	c.ServerEnvironment, err = parseServerEnvironment(os.Getenv("AGONES_FLEET_SERVER_ENV_JSON"))
	if err != nil {
		return c, err
	}
	c.Kubernetes, err = agones.FromEnv(c.DeploymentID, c.PortName, c.Mode == "local")
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.DeploymentID == "" || c.BuildHash == "" || len(c.BuildHash) > 256 || c.Region == "" || len(c.Region) > 128 || c.ControlURL == "" {
		return fmt.Errorf("deployment, build, region and control URL are required")
	}
	if len(c.SigningKey) != 32 || len(c.AdminToken) < 32 {
		return fmt.Errorf("signing key must be 32 bytes and admin token at least 32 characters")
	}
	if c.MaxRooms < 1 || c.MaxRooms > 512 || c.MaxInstances < 1 || c.MaxInstances > 1000 || c.MinInstances < 0 || c.MinInstances > c.MaxInstances {
		return fmt.Errorf("invalid capacity")
	}
	if c.Interval <= 0 || c.PrepareTimeout < 1 || c.PrepareTimeout > 120 || c.ReservationTTL != 60 || c.ReconnectSeconds < 1 || c.LaunchTimeout < 1 || c.AllocationTimeout < 1 || c.HeartbeatTimeout < 1 || c.LostTimeout <= c.HeartbeatTimeout || c.IdleSeconds < 1 || c.ProviderPollSeconds < 1 || c.MinLifetimeForAdmission < 60 {
		return fmt.Errorf("invalid lifecycle timeouts")
	}
	u, err := url.Parse(c.ControlURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid control URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.AllowHTTP && c.Mode == "local") {
		return fmt.Errorf("control URL requires HTTPS (HTTP is limited to explicit local mode)")
	}
	if c.Mode != "production" && c.Mode != "local" {
		return fmt.Errorf("unknown fleet mode")
	}
	if err := c.Kubernetes.Validate(); err != nil {
		return err
	}
	if c.Kubernetes.DeploymentID != c.DeploymentID || c.Kubernetes.PortName != c.PortName {
		return fmt.Errorf("Kubernetes profile differs from fleet identity")
	}
	return validateServerEnvironment(c.ServerEnvironment)
}

// This decoder deliberately rejects duplicate keys, non-string values and null.
// Never include JSON input or decoder errors in diagnostics: values can be secrets.
func parseServerEnvironment(raw string) (map[string]string, error) {
	if len(raw) > maxServerEnvironmentBytes || !utf8.ValidString(raw) {
		return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON must be valid UTF-8 within 32 KiB")
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON must be an object of string values")
	}
	values := make(map[string]string)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON contains invalid JSON")
		}
		name, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON contains an invalid variable name")
		}
		if _, exists := values[name]; exists {
			return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON contains duplicate variable names")
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON contains invalid JSON")
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON values must be strings")
		}
		values[name] = text
		if len(values) > maxServerEnvironmentEntries {
			return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON permits at most 64 variables")
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON contains invalid JSON")
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON contains trailing data")
	}
	if err = validateServerEnvironment(values); err != nil {
		return nil, err
	}
	return values, nil
}

func validateServerEnvironment(values map[string]string) error {
	if len(values) > maxServerEnvironmentEntries {
		return fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON permits at most 64 variables")
	}
	for name, value := range values {
		if len(name) == 0 || len(name) > 128 || strings.HasPrefix(strings.ToUpper(name), "AGONES_") || strings.HasPrefix(strings.ToUpper(name), "FLEET_") {
			return fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON contains an invalid or reserved variable name")
		}
		for i := 0; i < len(name); i++ {
			c := name[i]
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || i > 0 && c >= '0' && c <= '9') {
				return fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON variable names must match [A-Za-z_][A-Za-z0-9_]*")
			}
		}
		if len(value) > maxServerEnvironmentValueBytes || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON values must be UTF-8 without NUL and at most 8 KiB")
		}
	}
	encoded, err := json.Marshal(values)
	if err != nil || len(encoded) > maxServerEnvironmentBytes {
		return fmt.Errorf("AGONES_FLEET_SERVER_ENV_JSON encoded size must not exceed 32 KiB")
	}
	return nil
}

func (c Config) snapshot() Config {
	c.SigningKey = append([]byte(nil), c.SigningKey...)
	c.Kubernetes = c.Kubernetes.Snapshot()
	if c.ServerEnvironment != nil {
		values := make(map[string]string, len(c.ServerEnvironment))
		for name, value := range c.ServerEnvironment {
			values[name] = value
		}
		c.ServerEnvironment = values
	}
	return c
}

func env(key, fallback string) string {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		return s
	}
	return fallback
}
