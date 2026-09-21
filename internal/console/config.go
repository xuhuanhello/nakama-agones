package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config is private operator configuration, independent of Nakama's runtime.
type Config struct {
	Username         string       `json:"username"`
	PasswordHash     string       `json:"password_hash"`
	PublicURL        string       `json:"public_url"`
	Listen           string       `json:"listen"`
	Source           SourceConfig `json:"source"`
	ReadAPITokenHash string       `json:"read_api_token_hash,omitempty"`
}

const configSizeLimit = 64 << 10

// LoadConfig accepts a regular owner-only file. It never includes file contents
// or parser errors in the returned error, which can safely appear in service logs.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	before, err := os.Lstat(path)
	if err != nil || !privateFile(before) {
		return cfg, errors.New("configuration must be a regular 0600 file")
	}
	f, err := os.Open(path)
	if err != nil {
		return cfg, errors.New("cannot open configuration")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !privateFile(after) || !os.SameFile(before, after) {
		return cfg, errors.New("configuration file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, configSizeLimit+1))
	if err != nil || len(data) > configSizeLimit {
		return cfg, errors.New("cannot read configuration within size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid configuration JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, errors.New("invalid configuration JSON")
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func privateFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && info.Mode()&(os.ModeSetuid|os.ModeSetgid) == 0
}

func (cfg *Config) validate() error {
	if cfg.ReadAPITokenHash != "" && !validReadAPITokenHash(cfg.ReadAPITokenHash) {
		return errors.New("invalid read-only API token hash")
	}
	if !validUsername(cfg.Username) {
		return errors.New("invalid administrator username")
	}
	if _, _, _, ok := parsePasswordHash(cfg.PasswordHash); !ok {
		return errors.New("invalid administrator password hash")
	}
	if _, err := publicEndpoint(cfg.PublicURL); err != nil {
		return err
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:7365"
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 || !loopbackHost(host) {
		return errors.New("listen must be a loopback address with an explicit port")
	}
	return nil
}

func publicEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.Path != "/fleet-admin/" {
		return nil, errors.New("public_url must be an absolute /fleet-admin/ URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname())) {
		return nil, errors.New("public_url requires HTTPS or loopback HTTP")
	}
	if u.Host != strings.ToLower(u.Host) || strings.HasSuffix(u.Hostname(), ".") || strings.ContainsAny(u.Host, " \t\r\n\\%") {
		return nil, errors.New("public_url host must be canonical")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != u.Port() {
			return nil, errors.New("invalid public_url port")
		}
		// Browsers omit default ports from Origin. Reject ambiguous configuration
		// instead of relaxing the exact Origin comparison.
		if (u.Scheme == "https" && port == 443) || (u.Scheme == "http" && port == 80) {
			return nil, errors.New("public_url must omit the default port")
		}
	}
	return u, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validUsername(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for i, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i == 0 || !strings.ContainsRune("_.@-", rune(c)) {
			return false
		}
	}
	return true
}
