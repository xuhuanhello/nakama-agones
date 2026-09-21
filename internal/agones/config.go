// Package agones hosts independent GameServers through the Kubernetes REST API.
// It intentionally does not use FleetAutoscalers or GameServerAllocation: the
// Nakama manager owns process scaling and multiplexes rooms inside each process.
package agones

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const serviceAccountPath = "/var/run/secrets/kubernetes.io/serviceaccount/"

type Config struct {
	APIURL, TokenFile, CAFile                          string
	Namespace, Pool, DeploymentID, GameImage, PortName string
	GamePort                                           int
	CPURequest, CPULimit, MemoryRequest, MemoryLimit   string
	NodeSelector                                       map[string]string
	ImagePullSecret                                    string
	Local                                              bool
}

func FromEnv(deploymentID, portName string, local bool) (Config, error) {
	c := Config{
		APIURL:    value("AGONES_KUBERNETES_API_URL", "https://kubernetes.default.svc"),
		TokenFile: value("AGONES_KUBERNETES_TOKEN_FILE", serviceAccountPath+"token"),
		CAFile:    value("AGONES_KUBERNETES_CA_FILE", serviceAccountPath+"ca.crt"),
		Namespace: value("AGONES_NAMESPACE", "agones-games"), Pool: value("AGONES_POOL", "default"),
		DeploymentID: deploymentID, GameImage: os.Getenv("AGONES_GAME_IMAGE"), PortName: portName,
		CPURequest: value("AGONES_CPU_REQUEST", "500m"), MemoryRequest: value("AGONES_MEMORY_REQUEST", "256Mi"),
		CPULimit: os.Getenv("AGONES_CPU_LIMIT"), MemoryLimit: value("AGONES_MEMORY_LIMIT", "1Gi"),
		ImagePullSecret: os.Getenv("AGONES_IMAGE_PULL_SECRET"), Local: local,
	}
	var err error
	c.GamePort, err = strconv.Atoi(value("AGONES_GAME_PORT", "7770"))
	if err != nil {
		return c, errors.New("AGONES_GAME_PORT must be an integer")
	}
	if raw := os.Getenv("AGONES_NODE_SELECTOR_JSON"); raw != "" {
		if len(raw) > 4096 || json.Unmarshal([]byte(raw), &c.NodeSelector) != nil || c.NodeSelector == nil {
			return c, errors.New("AGONES_NODE_SELECTOR_JSON must be a small string map")
		}
	}
	return c, c.Validate()
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
var imageDigest = regexp.MustCompile(`^\S+@sha256:[a-f0-9]{64}$`)

func validLabel(s string) bool { return len(s) > 0 && len(s) <= 63 && dnsLabel.MatchString(s) }

func validLabelKey(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) > 2 {
		return false
	}
	name := parts[len(parts)-1]
	if len(name) == 0 || len(name) > 63 || !labelValue.MatchString(name) {
		return false
	}
	if len(parts) == 2 {
		if len(parts[0]) > 253 {
			return false
		}
		for _, p := range strings.Split(parts[0], ".") {
			if !validLabel(p) {
				return false
			}
		}
	}
	return true
}

func (c Config) Validate() error {
	u, err := url.Parse(c.APIURL)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.TrimRight(u.Path, "/") != "" {
		return errors.New("invalid Kubernetes API URL")
	}
	localHost := strings.EqualFold(u.Hostname(), "localhost")
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		localHost = ip.IsLoopback()
	}
	if u.Scheme != "https" && !(c.Local && localHost && u.Scheme == "http") {
		return errors.New("Kubernetes API requires HTTPS; explicit local mode permits loopback HTTP only")
	}
	if c.TokenFile == "" || !c.Local && c.CAFile == "" {
		return errors.New("Kubernetes token file and CA file are required")
	}
	if !validLabel(c.Namespace) || !validLabel(c.Pool) || c.DeploymentID == "" || len(c.DeploymentID) > 256 || !validLabel(c.PortName) || c.GamePort < 1 || c.GamePort > 65535 {
		return errors.New("invalid Agones namespace, pool, deployment or game port")
	}
	if strings.TrimSpace(c.GameImage) != c.GameImage || c.GameImage == "" || strings.ContainsAny(c.GameImage, "\r\n\t ") || !c.Local && !imageDigest.MatchString(c.GameImage) {
		return errors.New("AGONES_GAME_IMAGE must be a sha256 digest reference in production")
	}
	for _, q := range []string{c.CPURequest, c.CPULimit, c.MemoryRequest, c.MemoryLimit} {
		if q != "" {
			if _, valid := quantityValue(q); !valid {
				return errors.New("invalid Kubernetes resource quantity")
			}
		}
	}
	if len(c.NodeSelector) > 16 {
		return errors.New("too many node selectors")
	}
	for k, v := range c.NodeSelector {
		if !validLabelKey(k) || len(v) > 63 || !labelValue.MatchString(v) {
			return errors.New("invalid node selector")
		}
	}
	if os, exists := c.NodeSelector["kubernetes.io/os"]; exists && os != "linux" {
		return errors.New("Linux game image requires Linux nodes")
	}
	if c.ImagePullSecret != "" && !validLabel(c.ImagePullSecret) {
		return errors.New("invalid image pull secret name")
	}
	return nil
}

func (c Config) Snapshot() Config {
	copy := make(map[string]string, len(c.NodeSelector))
	for k, v := range c.NodeSelector {
		copy[k] = v
	}
	c.NodeSelector = copy
	return c
}

// Profile identifies immutable scheduling/build settings without transport
// credentials. Changing the API's endpoint or rotating its token does not alter
// game compatibility; moving to another cluster requires a new deployment ID.
func (c Config) Profile() any {
	return []any{c.Namespace, c.Pool, c.GameImage, c.GamePort, c.PortName, c.CPURequest, c.CPULimit, c.MemoryRequest, c.MemoryLimit, c.NodeSelector, c.ImagePullSecret}
}

func value(key, fallback string) string {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		return s
	}
	return fallback
}
