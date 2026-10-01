package gamefleet

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/heroiclabs/nakama-common/runtime"
)

var serviceEnvironmentNames = []string{
	"GAMEFLEET_SERVICE_URL",
	"GAMEFLEET_SERVICE_KEY_FILE",
	"GAMEFLEET_SERVICE_ID",
	"GAMEFLEET_SERVICE_APPLICATION_ID",
	"GAMEFLEET_SERVICE_IDENTITY_ISSUER",
	"GAMEFLEET_SERVICE_REGION",
	"GAMEFLEET_SERVICE_COMPATIBILITY",
}

// ServiceConfigFromEnv reads the independent gfsvc configuration. It never
// falls back to the ordinary caller key, placement, revision, or profile env.
func ServiceConfigFromEnv() (ServiceSearchConfig, error) {
	values := make([]string, len(serviceEnvironmentNames))
	for i, name := range serviceEnvironmentNames {
		values[i] = os.Getenv(name)
		if values[i] == "" {
			return ServiceSearchConfig{}, errors.New("GameFleet service settings must all be explicitly configured")
		}
	}

	path := values[1]
	info, err := os.Lstat(path)
	if err != nil || !privateServiceKeyFile(info) || info.Size() > 64 {
		return ServiceSearchConfig{}, errors.New("GAMEFLEET_SERVICE_KEY_FILE must be a private regular file (0400 or 0600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return ServiceSearchConfig{}, errors.New("cannot read GAMEFLEET_SERVICE_KEY_FILE")
	}
	defer f.Close()
	opened, err := f.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !privateServiceKeyFile(opened) || !privateServiceKeyFile(pathInfo) ||
		opened.Size() > 64 || !os.SameFile(info, opened) || !os.SameFile(pathInfo, opened) {
		return ServiceSearchConfig{}, errors.New("invalid GAMEFLEET_SERVICE_KEY_FILE")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil || len(raw) > 64 {
		return ServiceSearchConfig{}, errors.New("invalid GAMEFLEET_SERVICE_KEY_FILE")
	}
	key := string(raw)
	if strings.HasSuffix(key, "\r\n") {
		key = strings.TrimSuffix(key, "\r\n")
	} else if strings.HasSuffix(key, "\n") {
		key = strings.TrimSuffix(key, "\n")
	}
	if !validHistoryServiceKey(key) {
		return ServiceSearchConfig{}, errors.New("invalid GAMEFLEET_SERVICE_KEY_FILE credential")
	}
	return ServiceSearchConfig{
		URL: values[0], Key: key, ServiceID: values[2], ApplicationID: values[3],
		IdentityIssuer: values[4], Region: values[5], Compatibility: values[6],
	}, nil
}

func privateServiceKeyFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	mode := info.Mode().Perm()
	return mode == 0400 || mode == 0600
}

// Protocol selection is separate from service identity. Changing the wire
// protocol must not change the identity compared by the History transport.
func serviceSearchProtocolFromEnv() (string, error) {
	switch os.Getenv("GAMEFLEET_SERVICE_SEARCH_PROTOCOL") {
	case "", "v1":
		return ServiceSearchVersion, nil
	case "v2":
		return ServiceSearchRoutedVersion, nil
	default:
		return "", errors.New("GAMEFLEET_SERVICE_SEARCH_PROTOCOL must be v1 or v2")
	}
}

// RegisterServiceFromEnv configures the service-backed player bridge without
// constructing or migrating any fleet manager, database, or reconciler.
func RegisterServiceFromEnv(ctx context.Context, logger runtime.Logger, _ *sql.DB, _ runtime.NakamaModule, initializer runtime.Initializer) error {
	if initializer == nil {
		return errors.New("GameFleet service bridge requires an initializer")
	}
	protocol, err := serviceSearchProtocolFromEnv()
	if err != nil {
		return err
	}
	cfg, err := ServiceConfigFromEnv()
	if err != nil {
		return err
	}
	search, err := NewServiceSearchClient(cfg)
	if err != nil {
		return err
	}
	var routed *RoutedServiceSearchClient
	if protocol == ServiceSearchRoutedVersion {
		routed, err = NewRoutedServiceSearchClient(cfg)
		if err != nil {
			search.Close()
			return err
		}
	}
	history, err := NewServiceHistoryClient(ServiceHistoryConfig{
		URL: cfg.URL, Key: cfg.Key, ServiceID: cfg.ServiceID, ApplicationID: cfg.ApplicationID,
		IdentityIssuer: cfg.IdentityIssuer, Region: cfg.Region, Compatibility: cfg.Compatibility,
	})
	if err != nil {
		search.Close()
		routed.Close()
		return err
	}
	var archive *TerminalArchiveClient
	closeClients := func() {
		search.Close()
		routed.Close()
		history.Close()
		if archive != nil {
			archive.Close()
		}
	}
	archiveCfg, err := ArchiveConfigFromEnv()
	if err != nil {
		closeClients()
		return err
	}
	if archiveCfg != nil {
		archive, err = NewTerminalArchiveClient(*archiveCfg)
		if err != nil {
			closeClients()
			return err
		}
	}
	if err = search.CheckScope(ctx); err != nil {
		closeClients()
		return errors.New("GameFleet service search scope preflight failed")
	}
	if err = history.CheckScope(ctx); err != nil {
		closeClients()
		return errors.New("GameFleet history service scope preflight failed")
	}
	if archive != nil {
		if err = archive.CheckScope(ctx); err != nil {
			closeClients()
			return errors.New("GameFleet archive scope preflight failed")
		}
	}
	var routeClients []*RoutedServiceSearchClient
	if routed != nil {
		routeClients = append(routeClients, routed)
	}
	baseBackend, err := newServiceBackend(search, history, routeClients...)
	if err != nil {
		closeClients()
		return err
	}
	var backend Backend = baseBackend
	if archive != nil {
		backend = &terminalArchiveBackend{Backend: backend, archive: archive, archiveScope: *archiveCfg}
	}
	if err = registerServiceBridge(initializer, backend, search, routeClients...); err != nil {
		closeClients()
		return err
	}
	if err = initializer.RegisterShutdown(func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule) {
		closeClients()
	}); err != nil {
		closeClients()
		return err
	}
	if logger != nil {
		logger.Info("GameFleet service bridge registered with %s; allocation and capacity are owned by GameFleet", protocol)
	}
	return nil
}
