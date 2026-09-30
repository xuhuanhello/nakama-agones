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

// ConfigFromEnv reads a mounted private file; credentials are never bundled in
// the image or exposed by player RPCs. Configuration errors contain no values.
func ConfigFromEnv() (Config, error) {
	cfg := Config{URL: os.Getenv("GAMEFLEET_BUSINESS_URL"), ApplicationID: os.Getenv("GAMEFLEET_APPLICATION_ID"), PlacementID: os.Getenv("GAMEFLEET_PLACEMENT_ID"), RevisionID: os.Getenv("GAMEFLEET_REVISION_ID"), Region: os.Getenv("GAMEFLEET_REGION"), Compatibility: os.Getenv("GAMEFLEET_COMPATIBILITY")}
	path := os.Getenv("GAMEFLEET_BUSINESS_KEY_FILE")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4097 {
		return Config{}, errors.New("GAMEFLEET_BUSINESS_KEY_FILE must be a private regular file (0600 or 0400)")
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("cannot read GAMEFLEET_BUSINESS_KEY_FILE")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4098))
	if err != nil || len(raw) > 4097 {
		return Config{}, errors.New("invalid GameFleet business key file")
	}
	cfg.Key = strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	return cfg, nil
}

func RegisterFromEnv(ctx context.Context, logger runtime.Logger, _ *sql.DB, _ runtime.NakamaModule, initializer runtime.Initializer) error {
	cfg, err := ConfigFromEnv()
	if err != nil {
		return err
	}
	client, err := NewClient(cfg)
	if err != nil {
		return err
	}
	if err = client.CheckScope(ctx); err != nil {
		client.Close()
		return errors.New("GameFleet business scope preflight failed")
	}
	if err = Register(initializer, client, cfg); err != nil {
		client.Close()
		return err
	}
	if err = initializer.RegisterShutdown(func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule) { client.Close() }); err != nil {
		client.Close()
		return err
	}
	logger.Info("GameFleet pilot bridge registered; allocation and capacity are owned by GameFleet")
	return nil
}
