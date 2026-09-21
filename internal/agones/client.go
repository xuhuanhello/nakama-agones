package agones

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuhuanhello/nakama-agones/internal/provider"
)

const (
	managedLabel      = "app.kubernetes.io/managed-by"
	poolLabel         = "nakama-agones.io/pool"
	deploymentLabel   = "nakama-agones.io/deployment"
	workerLabel       = "nakama-agones.io/worker"
	ownerAnnotation   = "nakama-agones.io/deployment-id"
	buildAnnotation   = "nakama-agones.io/build-hash"
	profileAnnotation = "nakama-agones.io/config-hmac"
	maxBodyBytes      = 4 << 20
)

var workerID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var resourceUID = regexp.MustCompile(`^[A-Za-z0-9][-A-Za-z0-9]{0,127}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

type Client struct {
	cfg            Config
	base           *url.URL
	http           *http.Client
	deploymentHash string
}

var _ provider.Provider = (*Client)(nil)

// NewClient loads a trusted CA and refuses redirects. A service-account token is
// read for EVERY request, so projected-token rotation does not need a restart.
// The game image is an immutable digest in production; no floating version is
// inferred from a public registry or changed on a retries.
func NewClient(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, errors.New("cannot read Kubernetes CA file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid Kubernetes CA file")
		}
		tlsConfig.RootCAs = pool
	}
	base, _ := url.Parse(strings.TrimRight(cfg.APIURL, "/"))
	// The control plane is reached directly. An ambient proxy must not receive
	// a namespace service-account credential or turn internal calls into egress.
	transport := &http.Transport{
		TLSClientConfig:   tlsConfig,
		DialContext:       (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 8,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
	}
	hash := sha256.Sum256([]byte(cfg.DeploymentID))
	return &Client{cfg: cfg.Snapshot(), base: base, deploymentHash: hex.EncodeToString(hash[:16]), http: &http.Client{
		Timeout: 5 * time.Second, Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []ownerRef        `json:"ownerReferences,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
}
type ownerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}
type secret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   objectMeta        `json:"metadata"`
	Type       string            `json:"type"`
	Immutable  bool              `json:"immutable"`
	Data       map[string][]byte `json:"data"`
}
type gameServer struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   objectMeta `json:"metadata"`
	Spec       gameSpec   `json:"spec"`
	Status     gameStatus `json:"status,omitempty"`
}
type gameSpec struct {
	Container string         `json:"container"`
	Ports     []gamePort     `json:"ports"`
	Health    map[string]any `json:"health"`
	Template  podTemplate    `json:"template"`
}
type gamePort struct {
	Name          string `json:"name"`
	PortPolicy    string `json:"portPolicy"`
	ContainerPort int    `json:"containerPort"`
	HostPort      int    `json:"hostPort,omitempty"`
	Protocol      string `json:"protocol"`
}
type podTemplate struct {
	Metadata objectMeta `json:"metadata"`
	Spec     podSpec    `json:"spec"`
}
type podSpec struct {
	Containers                    []container         `json:"containers"`
	NodeSelector                  map[string]string   `json:"nodeSelector"`
	ImagePullSecrets              []map[string]string `json:"imagePullSecrets,omitempty"`
	TerminationGracePeriodSeconds int                 `json:"terminationGracePeriodSeconds"`
}
type container struct {
	Name            string         `json:"name"`
	Image           string         `json:"image"`
	ImagePullPolicy string         `json:"imagePullPolicy"`
	Env             []environment  `json:"env"`
	Resources       resources      `json:"resources"`
	SecurityContext map[string]any `json:"securityContext"`
}
type resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}
type environment struct {
	Name      string       `json:"name"`
	Value     string       `json:"value,omitempty"`
	ValueFrom *secretValue `json:"valueFrom,omitempty"`
}
type secretValue struct {
	SecretKeyRef secretRef `json:"secretKeyRef"`
}
type secretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}
type gameStatus struct {
	State   string `json:"state"`
	Address string `json:"address"`
	Ports   []struct {
		Name string `json:"name"`
		Port int    `json:"port"`
	} `json:"ports"`
}

func (c *Client) gamePath() string {
	return "/apis/agones.dev/v1/namespaces/" + c.cfg.Namespace + "/gameservers"
}
func (c *Client) secretPath() string { return "/api/v1/namespaces/" + c.cfg.Namespace + "/secrets" }
func gameName(worker string) string  { return "nag-" + worker }
func secretName(name string) string  { return name + "-credentials" }

func (c *Client) labels(worker string) map[string]string {
	return map[string]string{managedLabel: "nakama-agones", poolLabel: c.cfg.Pool, deploymentLabel: c.deploymentHash, workerLabel: worker}
}

func (c *Client) desired(req provider.StartRequest) (gameServer, secret, error) {
	worker, _ := req.CustomData["worker_id"].(string)
	owner, _ := req.CustomData["fleet_owner"].(string)
	build, _ := req.CustomData["build_hash"].(string)
	if !workerID.MatchString(worker) || owner != c.cfg.DeploymentID || build == "" || len(build) > 256 || req.Region == "" || req.Name != gameName(worker) {
		return gameServer{}, secret{}, failure("start", 0, "invalid_worker_identity", false)
	}
	if req.EnvironmentVariables["AGONES_FLEET_WORKER_ID"] != worker || req.EnvironmentVariables["AGONES_FLEET_BUILD_HASH"] != build || req.EnvironmentVariables["AGONES_FLEET_BOOTSTRAP_TOKEN"] == "" || req.EnvironmentVariables["AGONES_FLEET_ADMISSION_KEY"] == "" {
		return gameServer{}, secret{}, failure("start", 0, "missing_worker_credentials", false)
	}
	if len(req.EnvironmentVariables) > 80 {
		return gameServer{}, secret{}, failure("start", 0, "too_many_environment_variables", false)
	}
	data := make(map[string][]byte, len(req.EnvironmentVariables))
	keys := make([]string, 0, len(req.EnvironmentVariables))
	for k, v := range req.EnvironmentVariables {
		if !envName.MatchString(k) || len(v) > 8192 || strings.ContainsRune(v, 0) {
			return gameServer{}, secret{}, failure("start", 0, "invalid_environment_variable", false)
		}
		data[k] = []byte(v)
		keys = append(keys, k)
	}
	sort.Strings(keys)
	nodeSelector := map[string]string{"kubernetes.io/os": "linux"}
	for k, v := range c.cfg.NodeSelector {
		nodeSelector[k] = v
	}
	game := gameServer{APIVersion: "agones.dev/v1", Kind: "GameServer", Metadata: objectMeta{Name: req.Name, Namespace: c.cfg.Namespace, Labels: c.labels(worker), Annotations: map[string]string{ownerAnnotation: owner, buildAnnotation: build}}, Spec: gameSpec{
		Container: "game", Ports: []gamePort{{Name: c.cfg.PortName, PortPolicy: "Dynamic", ContainerPort: c.cfg.GamePort, Protocol: "UDP"}},
		Health:   map[string]any{"disabled": false, "initialDelaySeconds": 30, "periodSeconds": 5, "failureThreshold": 3},
		Template: podTemplate{Metadata: objectMeta{Labels: c.labels(worker)}, Spec: podSpec{NodeSelector: nodeSelector, TerminationGracePeriodSeconds: 60}},
	}}
	gameContainer := container{Name: "game", Image: c.cfg.GameImage, ImagePullPolicy: "IfNotPresent", Env: []environment{}, Resources: resources{Requests: map[string]string{}, Limits: map[string]string{}}, SecurityContext: map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}}}
	for _, k := range keys {
		gameContainer.Env = append(gameContainer.Env, environment{Name: k, ValueFrom: &secretValue{secretRef{Name: secretName(req.Name), Key: k}}})
	}
	for k, v := range map[string]string{"cpu": c.cfg.CPURequest, "memory": c.cfg.MemoryRequest} {
		if v != "" {
			gameContainer.Resources.Requests[k] = v
		}
	}
	for k, v := range map[string]string{"cpu": c.cfg.CPULimit, "memory": c.cfg.MemoryLimit} {
		if v != "" {
			gameContainer.Resources.Limits[k] = v
		}
	}
	game.Spec.Template.Spec.Containers = []container{gameContainer}
	if c.cfg.ImagePullSecret != "" {
		game.Spec.Template.Spec.ImagePullSecrets = []map[string]string{{"name": c.cfg.ImagePullSecret}}
	}
	// Key the fingerprint with a per-worker secret: custom low-entropy env values
	// must not become guessable from publicly readable GameServer annotations.
	payload, _ := json.Marshal([]any{game.Spec, req.Region, data})
	h := hmac.New(sha256.New, []byte(req.EnvironmentVariables["AGONES_FLEET_BOOTSTRAP_TOKEN"]))
	_, _ = h.Write(payload)
	profile := hex.EncodeToString(h.Sum(nil))
	game.Metadata.Annotations[profileAnnotation] = profile
	credentials := secret{APIVersion: "v1", Kind: "Secret", Type: "Opaque", Immutable: true, Metadata: objectMeta{Name: secretName(req.Name), Namespace: c.cfg.Namespace, Labels: c.labels(worker), Annotations: map[string]string{ownerAnnotation: owner, buildAnnotation: build, profileAnnotation: profile}}, Data: data}
	return game, credentials, nil
}

func (c *Client) Start(ctx context.Context, req provider.StartRequest) (provider.Instance, error) {
	desired, credentials, err := c.desired(req)
	if err != nil {
		return provider.Instance{}, err
	}
	// A deterministic Secret is created before the pod. No credential values are
	// ever in GameServer annotations, logs, list responses or literal env entries.
	var existingSecret secret
	err = c.request(ctx, "secret_create", http.MethodPost, c.secretPath(), nil, credentials, &existingSecret)
	if isStatus(err, 409) || provider.IsOutcomeUnknown(err) {
		original := err
		err = c.request(ctx, "secret_reconcile", http.MethodGet, c.secretPath()+"/"+credentials.Metadata.Name, nil, nil, &existingSecret)
		if err != nil && provider.IsOutcomeUnknown(original) {
			return provider.Instance{}, uncertain(original)
		}
	}
	if err != nil {
		return provider.Instance{}, err
	}
	if !c.ownedSecret(existingSecret, desired) || !reflect.DeepEqual(existingSecret.Data, credentials.Data) {
		return provider.Instance{}, failure("start", 409, "secret_identity_or_config_conflict", false)
	}
	if len(existingSecret.Metadata.OwnerReferences) > 0 {
		refs := existingSecret.Metadata.OwnerReferences
		if len(refs) != 1 || refs[0].APIVersion != "agones.dev/v1" || refs[0].Kind != "GameServer" || refs[0].Name != desired.Metadata.Name {
			return provider.Instance{}, failure("start", 409, "secret_owner_conflict", false)
		}
		var owner gameServer
		if e := c.request(ctx, "secret_owner_check", http.MethodGet, c.gamePath()+"/"+desired.Metadata.Name, nil, nil, &owner); e != nil {
			if isStatus(e, 404) {
				return provider.Instance{}, failure("start", 409, "secret_owner_conflict", false)
			}
			return provider.Instance{}, e
		}
		if owner.Metadata.UID != refs[0].UID || !c.sameGame(owner, desired) {
			return provider.Instance{}, failure("start", 409, "secret_owner_conflict", false)
		}
	}
	var actual gameServer
	err = c.request(ctx, "start", http.MethodPost, c.gamePath(), nil, desired, &actual)
	if isStatus(err, 409) || provider.IsOutcomeUnknown(err) {
		original := err
		err = c.request(ctx, "create_reconcile", http.MethodGet, c.gamePath()+"/"+desired.Metadata.Name, nil, nil, &actual)
		if err != nil && provider.IsOutcomeUnknown(original) {
			return provider.Instance{}, uncertain(original)
		}
	}
	if err != nil {
		// A definitive invalid spec cannot launch later. Retain uncertain creates
		// for reconciliation; never delete credentials from a possibly live pod.
		if !provider.IsOutcomeUnknown(err) && (isStatus(err, 400) || isStatus(err, 403) || isStatus(err, 422)) {
			var check gameServer
			if e := c.request(ctx, "failed_create_check", http.MethodGet, c.gamePath()+"/"+desired.Metadata.Name, nil, nil, &check); isStatus(e, 404) {
				_ = c.deleteSecret(ctx, existingSecret)
			}
		}
		return provider.Instance{}, err
	}
	if !c.sameGame(actual, desired) {
		return provider.Instance{}, failure("start", 409, "gameserver_identity_or_config_conflict", false)
	}
	if err = c.ensureSecretOwner(ctx, actual, &existingSecret); err != nil {
		return provider.Instance{}, uncertain(err)
	}
	return c.instance(actual), nil
}

func (c *Client) owned(meta objectMeta) bool {
	worker := meta.Labels[workerLabel]
	return meta.Namespace == c.cfg.Namespace && workerID.MatchString(worker) && meta.Labels[managedLabel] == "nakama-agones" && meta.Labels[poolLabel] == c.cfg.Pool && meta.Labels[deploymentLabel] == c.deploymentHash && meta.Annotations[ownerAnnotation] == c.cfg.DeploymentID
}

func (c *Client) ownedGame(g gameServer) bool {
	return g.APIVersion == "agones.dev/v1" && g.Kind == "GameServer" && len(g.Metadata.OwnerReferences) == 0 && c.owned(g.Metadata) && g.Metadata.Name == gameName(g.Metadata.Labels[workerLabel]) && resourceUID.MatchString(g.Metadata.UID) && g.Metadata.Annotations[profileAnnotation] != "" && g.Metadata.Annotations[buildAnnotation] != ""
}

func (c *Client) ownedSecret(s secret, g gameServer) bool {
	return s.APIVersion == "v1" && s.Kind == "Secret" && c.owned(s.Metadata) && s.Metadata.Name == secretName(g.Metadata.Name) && resourceUID.MatchString(s.Metadata.UID) && s.Immutable && s.Type == "Opaque" && s.Metadata.DeletionTimestamp == "" && s.Metadata.Annotations[profileAnnotation] == g.Metadata.Annotations[profileAnnotation]
}

func (c *Client) sameGame(g, want gameServer) bool {
	if !c.ownedGame(g) || g.Metadata.DeletionTimestamp != "" || g.Metadata.Name != want.Metadata.Name || g.Metadata.Annotations[profileAnnotation] != want.Metadata.Annotations[profileAnnotation] || g.Metadata.Annotations[buildAnnotation] != want.Metadata.Annotations[buildAnnotation] {
		return false
	}
	// Compare the immutable launch settings as well as the keyed fingerprint.
	// Kubernetes adds hostPort and default fields, so compare only the declared
	// port contract and container settings the controller owns.
	if g.Spec.Container != want.Spec.Container || len(g.Spec.Ports) != 1 || len(g.Spec.Template.Spec.Containers) != 1 {
		return false
	}
	p, w := g.Spec.Ports[0], want.Spec.Ports[0]
	if p.Name != w.Name || p.PortPolicy != w.PortPolicy || p.Protocol != w.Protocol || p.ContainerPort != w.ContainerPort {
		return false
	}
	gc, wc := g.Spec.Template.Spec.Containers[0], want.Spec.Template.Spec.Containers[0]
	if g.Spec.Template.Spec.TerminationGracePeriodSeconds != want.Spec.Template.Spec.TerminationGracePeriodSeconds || !equalJSON(gc.SecurityContext, wc.SecurityContext) {
		return false
	}
	if disabled, _ := g.Spec.Health["disabled"].(bool); disabled {
		return false
	}
	for _, key := range []string{"initialDelaySeconds", "periodSeconds", "failureThreshold"} {
		if !equalJSON(g.Spec.Health[key], want.Spec.Health[key]) {
			return false
		}
	}
	return gc.Name == wc.Name && gc.Image == wc.Image && gc.ImagePullPolicy == wc.ImagePullPolicy && reflect.DeepEqual(gc.Env, wc.Env) && sameQuantities(gc.Resources.Requests, wc.Resources.Requests) && sameQuantities(gc.Resources.Limits, wc.Resources.Limits) && equalJSON(g.Spec.Template.Spec.NodeSelector, want.Spec.Template.Spec.NodeSelector) && equalJSON(g.Spec.Template.Spec.ImagePullSecrets, want.Spec.Template.Spec.ImagePullSecrets)
}

// Kubernetes canonicalizes quantities, e.g. CPU 0.5 -> 500m and 1024Mi -> 1Gi.
// Compare exact rational values instead of rejecting our own admitted object.
var quantityParts = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([numkKMGTP]i?|[eE][+-]?[0-9]+)?$`)

func quantityValue(raw string) (*big.Rat, bool) {
	parts := quantityParts.FindStringSubmatch(raw)
	if parts == nil || len(raw) > 32 {
		return nil, false
	}
	value, ok := new(big.Rat).SetString(parts[1])
	if !ok {
		return nil, false
	}
	unit := parts[2]
	exponent, base := 0, int64(10)
	if len(unit) > 1 && (unit[0] == 'e' || unit[0] == 'E') {
		var err error
		exponent, err = strconv.Atoi(unit[1:])
		if err != nil || exponent < -30 || exponent > 30 {
			return nil, false
		}
	} else if unit != "" {
		powers := map[byte]int{'n': -9, 'u': -6, 'm': -3, 'k': 3, 'K': 3, 'M': 6, 'G': 9, 'T': 12, 'P': 15}
		var exists bool
		exponent, exists = powers[unit[0]]
		if !exists {
			return nil, false
		}
		if strings.HasSuffix(unit, "i") {
			if exponent < 3 {
				return nil, false
			}
			base, exponent = 2, exponent/3*10
		}
	}
	factor := new(big.Int).Exp(big.NewInt(base), big.NewInt(int64(max(exponent, -exponent))), nil)
	if exponent < 0 {
		value.Quo(value, new(big.Rat).SetInt(factor))
	} else {
		value.Mul(value, new(big.Rat).SetInt(factor))
	}
	return value, true
}

func sameQuantities(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, raw := range a {
		av, ok := quantityValue(raw)
		bv, bok := quantityValue(b[key])
		if !ok || !bok || av.Cmp(bv) != 0 {
			return false
		}
	}
	return true
}

func equalJSON(a, b any) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(aa, bb)
}

func parseID(id string) (string, string, bool) {
	parts := strings.Split(id, "@")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "nag-") || !workerID.MatchString(strings.TrimPrefix(parts[0], "nag-")) || !resourceUID.MatchString(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (c *Client) get(ctx context.Context, id string) (gameServer, error) {
	name, uid, ok := parseID(id)
	if !ok {
		return gameServer{}, failure("get", 0, "invalid_instance_id", false)
	}
	var g gameServer
	if err := c.request(ctx, "get", http.MethodGet, c.gamePath()+"/"+name, nil, nil, &g); err != nil {
		return g, err
	}
	if !c.ownedGame(g) || g.Metadata.Name != name || g.Metadata.UID != uid {
		return g, failure("get", 409, "resource_identity_mismatch", false)
	}
	return g, nil
}

func (c *Client) Get(ctx context.Context, id string) (provider.Instance, error) {
	g, err := c.get(ctx, id)
	if err != nil {
		return provider.Instance{}, err
	}
	return c.instance(g), nil
}

func (c *Client) List(ctx context.Context) ([]provider.Instance, error) {
	instances := []provider.Instance{}
	selector := managedLabel + "=nakama-agones," + poolLabel + "=" + c.cfg.Pool + "," + deploymentLabel + "=" + c.deploymentHash
	seen := map[string]bool{}
	seenCursors := map[string]bool{}
	cursor := ""
	for page := 0; page < 100; page++ {
		q := url.Values{"labelSelector": {selector}, "limit": {"100"}}
		if cursor != "" {
			q.Set("continue", cursor)
		}
		var result struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []gameServer `json:"items"`
		}
		if err := c.request(ctx, "list", http.MethodGet, c.gamePath(), q, nil, &result); err != nil {
			return nil, err
		}
		if result.Items == nil {
			return nil, failure("list", 0, "invalid_list_response", false)
		}
		for _, g := range result.Items {
			if !c.ownedGame(g) {
				continue
			} // Do not trust a server-side selector alone.
			id := g.Metadata.Name + "@" + g.Metadata.UID
			if seen[id] {
				continue
			}
			seen[id] = true
			instances = append(instances, c.instance(g))
		}
		cursor = result.Metadata.Continue
		if cursor == "" {
			return instances, nil
		}
		if seenCursors[cursor] || len(cursor) > 4096 {
			return nil, failure("list", 0, "invalid_pagination", false)
		}
		seenCursors[cursor] = true
	}
	return nil, failure("list", 0, "pagination_limit_exceeded", false)
}

func (c *Client) instance(g gameServer) provider.Instance {
	state := "launching"
	switch g.Status.State {
	case "Allocated":
		state = "running"
	case "Unhealthy", "Error":
		state = "unhealthy"
	case "Shutdown":
		state = "unhealthy" // Wait for confirmed deletion before releasing capacity.
	}
	if g.Metadata.DeletionTimestamp != "" {
		state = "unhealthy"
	}
	i := provider.Instance{InstanceID: g.Metadata.Name + "@" + g.Metadata.UID, Status: state, CustomData: map[string]any{"fleet_owner": c.cfg.DeploymentID, "worker_id": g.Metadata.Labels[workerLabel], "build_hash": g.Metadata.Annotations[buildAnnotation]}}
	if state == "running" {
		for _, p := range g.Status.Ports {
			if p.Name == c.cfg.PortName {
				i.NetworkPorts = append(i.NetworkPorts, provider.Port{Name: p.Name, Host: g.Status.Address, ExternalPort: p.Port, InternalPort: c.cfg.GamePort, Protocol: "udp"})
			}
		}
	}
	return i
}

func (c *Client) ensureSecretOwner(ctx context.Context, g gameServer, known *secret) error {
	var s secret
	if known != nil {
		s = *known
	} else if err := c.request(ctx, "secret_get", http.MethodGet, c.secretPath()+"/"+secretName(g.Metadata.Name), nil, nil, &s); err != nil {
		return err
	}
	if !c.ownedSecret(s, g) {
		return failure("secret_owner", 409, "secret_identity_mismatch", false)
	}
	expected := ownerRef{APIVersion: "agones.dev/v1", Kind: "GameServer", Name: g.Metadata.Name, UID: g.Metadata.UID}
	if len(s.Metadata.OwnerReferences) > 0 {
		if len(s.Metadata.OwnerReferences) != 1 || s.Metadata.OwnerReferences[0] != expected {
			return failure("secret_owner", 409, "secret_owner_conflict", false)
		}
		return nil
	}
	if s.Metadata.ResourceVersion == "" {
		return failure("secret_owner", 0, "missing_resource_version", false)
	}
	s.Metadata.OwnerReferences = []ownerRef{expected}
	// Full update includes resourceVersion: concurrent owner changes cannot be
	// silently overwritten. Secret data are immutable and never logged.
	var updated secret
	if err := c.request(ctx, "secret_owner", http.MethodPut, c.secretPath()+"/"+s.Metadata.Name, nil, s, &updated); err != nil {
		return err
	}
	if updated.Metadata.UID != s.Metadata.UID || len(updated.Metadata.OwnerReferences) != 1 || updated.Metadata.OwnerReferences[0] != expected {
		return failure("secret_owner", 0, "invalid_owner_response", false)
	}
	return nil
}

func (c *Client) Stop(ctx context.Context, id string) error {
	g, err := c.get(ctx, id)
	if isStatus(err, 404) {
		// OwnerReference garbage collection normally removes this Secret. Also
		// clean it explicitly if a previous metadata update was interrupted.
		name, uid, ok := parseID(id)
		if !ok {
			return err
		}
		return c.cleanupAbsentSecret(ctx, name, uid)
	}
	if err != nil {
		return err
	}
	deleteOptions := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": g.Metadata.UID}, "propagationPolicy": "Foreground"}
	err = c.request(ctx, "stop", http.MethodDelete, c.gamePath()+"/"+g.Metadata.Name, nil, deleteOptions, nil)
	if isStatus(err, 404) {
		return c.cleanupAbsentSecret(ctx, g.Metadata.Name, g.Metadata.UID)
	}
	return err
}

func (c *Client) cleanupAbsentSecret(ctx context.Context, name, uid string) error {
	var s secret
	err := c.request(ctx, "secret_cleanup_get", http.MethodGet, c.secretPath()+"/"+secretName(name), nil, nil, &s)
	if isStatus(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	if !c.owned(s.Metadata) || s.Metadata.Name != secretName(name) || !resourceUID.MatchString(s.Metadata.UID) {
		return failure("secret_cleanup", 409, "secret_identity_mismatch", false)
	}
	if len(s.Metadata.OwnerReferences) != 1 || s.Metadata.OwnerReferences[0] != (ownerRef{APIVersion: "agones.dev/v1", Kind: "GameServer", Name: name, UID: uid}) {
		return failure("secret_cleanup", 409, "secret_owner_conflict", false)
	}
	return c.deleteSecret(ctx, s)
}

func (c *Client) deleteSecret(ctx context.Context, s secret) error {
	options := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": s.Metadata.UID}}
	err := c.request(ctx, "secret_cleanup", http.MethodDelete, c.secretPath()+"/"+s.Metadata.Name, nil, options, nil)
	if isStatus(err, 404) {
		return nil
	}
	return err
}

func failure(op string, status int, reason string, unknown bool) *provider.APIError {
	return &provider.APIError{Operation: op, StatusCode: status, Reason: reason, OutcomeUnknown: unknown}
}
func isStatus(err error, status int) bool {
	var e *provider.APIError
	return errors.As(err, &e) && e.StatusCode == status
}
func uncertain(err error) error {
	var e *provider.APIError
	if errors.As(err, &e) {
		copy := *e
		copy.OutcomeUnknown = true
		return &copy
	}
	return failure("start", 0, "create_outcome_unknown", true)
}

func (c *Client) request(ctx context.Context, op, method, path string, query url.Values, input, output any) error {
	if err := ctx.Err(); err != nil {
		e := failure(op, 0, "context_ended", false)
		e.Cause = err
		return e
	}
	token, err := os.ReadFile(c.cfg.TokenFile)
	if err != nil {
		return failure(op, 0, "token_file_unavailable", false)
	}
	token = bytes.TrimSpace(token)
	if len(token) == 0 || len(token) > 64<<10 || bytes.ContainsAny(token, "\r\n \t") {
		return failure(op, 0, "invalid_token_file", false)
	}
	var data []byte
	if input != nil {
		data, err = json.Marshal(input)
		if err != nil || len(data) > maxBodyBytes {
			return failure(op, 0, "invalid_request_body", false)
		}
	}
	u := *c.base
	u.Path = path
	u.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(data))
	if err != nil {
		return failure(op, 0, "invalid_request", false)
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		e := failure(op, 0, "transport_error", method == http.MethodPost)
		if errors.Is(err, context.Canceled) {
			e.Cause = context.Canceled
		} else if errors.Is(err, context.DeadlineExceeded) {
			e.Cause = context.DeadlineExceeded
		}
		return e
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxBodyBytes))
		e := failure(op, response.StatusCode, "kubernetes_rejected_request", method == http.MethodPost && (response.StatusCode >= 500 || response.StatusCode == 408))
		if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
			e.RetryAfter = time.Duration(min(seconds, 3600)) * time.Second
		}
		return e
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return failure(op, response.StatusCode, "invalid_response_body", method == http.MethodPost)
	}
	if output != nil && json.Unmarshal(body, output) != nil {
		return failure(op, response.StatusCode, "invalid_response_json", method == http.MethodPost)
	}
	return nil
}
