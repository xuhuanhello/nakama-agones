package fleetmanager

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/xuhuanhello/nakama-agones/internal/agones"
	"github.com/xuhuanhello/nakama-agones/internal/state"
	"github.com/xuhuanhello/nakama-agones/pkg/gamefleet"
)

// NewFromEnv creates a manager without installing hooks. Run the fleet DB
// migration first. The caller owns close until Register succeeds; afterward the
// manager closes its DB when Run exits during Nakama shutdown.
func NewFromEnv(ctx context.Context) (manager *Manager, close func(), err error) {
	cfg, err := FromEnv()
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	close = func() { _ = db.Close() }
	defer func() {
		if err != nil {
			close()
		}
	}()
	setup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	store, err := state.NewPostgres(setup, db, cfg.DeploymentID)
	if err != nil {
		return nil, close, err
	}
	provider, err := agones.NewClient(cfg.Kubernetes)
	if err != nil {
		return nil, close, err
	}
	manager, err = New(cfg, store, provider)
	if err != nil {
		return nil, close, err
	}
	manager.onStop = close
	return manager, close, nil
}

// RegisterFromEnv is the public library entry point for the included two-player
// bridge. It owns the matchmaker-matched hook; compose existing game hooks first.
func RegisterFromEnv(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer) error {
	switch os.Getenv("NAKAMA_FLEET_BACKEND") {
	case "gamefleet-service":
		return gamefleet.RegisterServiceFromEnv(ctx, logger, db, nk, initializer)
	case "gamefleet":
		return gamefleet.RegisterFromEnv(ctx, logger, db, nk, initializer)
	case "", "agones":
		// Legacy remains the default until the player pilot is accepted.
	default:
		return fmt.Errorf("unsupported NAKAMA_FLEET_BACKEND")
	}
	m, close, err := NewFromEnv(ctx)
	if err != nil {
		return err
	}
	if err = Register(ctx, logger, db, nk, initializer, m); err != nil {
		close()
		return err
	}
	return nil
}
