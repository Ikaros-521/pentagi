package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"pentagi/pkg/config"
	"pentagi/pkg/database"
	"pentagi/pkg/providers/custom"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"

	"github.com/sirupsen/logrus"
)

// CustomFileChannelsConfigured reports whether the custom provider config file
// defines at least one named channel, so callers (UI availability flags) can
// treat the custom type as usable even without LLM_SERVER_* environment
// variables.
func CustomFileChannelsConfigured(cfg *config.Config) bool {
	entries, err := loadNamedCustomChannels(cfg)
	return err == nil && len(entries) > 0
}

// loadNamedCustomChannels reads and parses the "providers:" list from the
// custom provider config file (LLM_SERVER_CONFIG_PATH).
func loadNamedCustomChannels(cfg *config.Config) ([]*pconfig.ProviderConfig, error) {
	if cfg == nil || cfg.LLMServerConfig == "" {
		return nil, nil
	}

	data, err := os.ReadFile(cfg.LLMServerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to read custom provider config file: %w", err)
	}

	entries, err := pconfig.LoadNamedProviders(data, custom.BaseDefaultOptions())
	if err != nil {
		return nil, fmt.Errorf("failed to parse named providers: %w", err)
	}

	return entries, nil
}

// seedCustomFileProviders upserts every named channel from the custom provider
// config file into the database for all users. Environment variables remain the
// bootstrap for the default channel; named channels live in configuration and
// are (re-)applied on every startup and config-file change, so adding a channel
// is a file edit — no env vars, no redeploy. Channels removed from the file are
// deliberately kept in the database; delete them in the UI.
func (pc *providerController) seedCustomFileProviders(ctx context.Context) error {
	entries, err := loadNamedCustomChannels(pc.cfg)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	users, err := pc.db.GetUsers(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch users for custom provider seeding: %w", err)
	}

	for _, u := range users {
		for _, entry := range entries {
			rawConfig, err := json.Marshal(entry)
			if err != nil {
				return fmt.Errorf("failed to marshal named provider %q: %w", entry.Name, err)
			}

			existing, err := pc.db.GetUserProviderByName(ctx, database.GetUserProviderByNameParams{
				Name:   entry.Name,
				UserID: u.ID,
			})
			if err != nil && err != sql.ErrNoRows {
				return fmt.Errorf("failed to get provider %q from database: %w", entry.Name, err)
			}

			if err == sql.ErrNoRows {
				if _, err := pc.db.CreateProvider(ctx, database.CreateProviderParams{
					UserID: u.ID,
					Type:   database.ProviderType(provider.ProviderCustom),
					Name:   entry.Name,
					Config: rawConfig,
				}); err != nil {
					return fmt.Errorf("failed to create named provider %q: %w", entry.Name, err)
				}
				logrus.WithFields(logrus.Fields{
					"provider": entry.Name,
					"user_id":  u.ID,
				}).Info("custom channel from config file was registered")
				continue
			}

			if existing.Type != database.ProviderType(provider.ProviderCustom) {
				logrus.WithFields(logrus.Fields{
					"provider": entry.Name,
					"user_id":  u.ID,
					"type":     string(existing.Type),
				}).Warn("a provider with this name already exists with another type, config file entry skipped")
				continue
			}

			if _, err := pc.db.UpdateUserProvider(ctx, database.UpdateUserProviderParams{
				ID:     existing.ID,
				UserID: u.ID,
				Name:   existing.Name,
				Config: rawConfig,
			}); err != nil {
				return fmt.Errorf("failed to update named provider %q: %w", entry.Name, err)
			}
			logrus.WithFields(logrus.Fields{
				"provider": entry.Name,
				"user_id":  u.ID,
			}).Info("custom channel from config file was updated")
		}
	}

	return nil
}

// watchCustomFileProviders re-applies named channels whenever the config file
// changes on disk, so a channel added to the YAML shows up without a restart.
// LLM_SERVER_CONFIG_RELOAD controls the poll interval in seconds (0 disables
// the watcher).
func (pc *providerController) watchCustomFileProviders(ctx context.Context) {
	interval := time.Duration(pc.cfg.LLMServerConfigReload) * time.Second
	if interval <= 0 || pc.cfg.LLMServerConfig == "" {
		return
	}

	lastMod := time.Time{}
	if fi, err := os.Stat(pc.cfg.LLMServerConfig); err == nil {
		lastMod = fi.ModTime()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		fi, err := os.Stat(pc.cfg.LLMServerConfig)
		if err != nil || fi.ModTime().Equal(lastMod) {
			continue
		}
		lastMod = fi.ModTime()

		if err := pc.seedCustomFileProviders(ctx); err != nil {
			logrus.WithError(err).Warn("failed to re-apply custom channels from config file")
		}
	}
}
