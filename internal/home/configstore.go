package home

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/AdguardTeam/golibs/log"
)

const (
	envConfigPostgresEnabled = "NULLPRIVATE_CONFIG_POSTGRES_ENABLED"
	envConfigPostgresDSN     = "NULLPRIVATE_CONFIG_POSTGRES_DSN"

	configRevisionRetentionLimit = 200
	defaultConfigRevisionActor   = "system"
)

type configChangeReason string

const (
	configChangeReasonBootstrap    configChangeReason = "bootstrap"
	configChangeReasonUpdate       configChangeReason = "update"
	configChangeReasonMigrate      configChangeReason = "migrate"
	configChangeReasonRollback     configChangeReason = "rollback"
	configChangeReasonLegacyImport configChangeReason = "legacy_import"
)

// ConfigRevisionMeta describes a stored configuration revision.
type ConfigRevisionMeta struct {
	RevisionID           int64
	ParentRevisionID     *int64
	RollbackOfRevisionID *int64
	SchemaVersion        uint
	SHA256               string
	ChangeReason         string
	CreatedBy            string
	CreatedAt            time.Time
}

// configStore stores the persisted application configuration.
type configStore interface {
	Exists() (ok bool, err error)
	Read() (data []byte, err error)
	Write(data []byte) (err error)
	BootstrapFromFile(filePath string) (imported bool, err error)
	Close() (err error)
}

// versionedConfigStore exposes revision history for configuration backends that
// support it.
type versionedConfigStore interface {
	configStore
	ActiveRevision() (meta ConfigRevisionMeta, err error)
	ListRevisions(limit int, beforeRevisionID int64) (items []ConfigRevisionMeta, err error)
	ReadRevision(revisionID int64) (data []byte, meta ConfigRevisionMeta, err error)
	RollbackTo(revisionID int64, actor string) (newActive ConfigRevisionMeta, err error)
}

type configStoreWriteWithReason interface {
	WriteWithReason(data []byte, reason configChangeReason, actor string) (err error)
}

type configMirrorWriter interface {
	Write(data []byte) (err error)
}

var (
	newPostgresConfigStoreFunc = func(ctx context.Context, dsn string) (store configStore, err error) {
		return newPostgresConfigStore(ctx, dsn)
	}

	newConfigMirrorWriter = func(filePath string) (writer configMirrorWriter) {
		return newFileConfigStore(filePath)
	}
)

// initConfigStore initializes the active configuration store.
func initConfigStore() (err error) {
	if globalContext.configStore != nil {
		err = globalContext.configStore.Close()
		if err != nil {
			return fmt.Errorf("closing previous config store: %w", err)
		}
	}

	globalContext.configStore, err = newConfigStore(context.Background())
	if err != nil {
		return err
	}

	return nil
}

// newConfigStore creates a configuration store selected by environment
// variables.
func newConfigStore(ctx context.Context) (store configStore, err error) {
	enabled, err := isPostgresConfigStoreEnabled()
	if err != nil {
		return nil, err
	}

	if !enabled {
		return newFileConfigStore(configFilePath()), nil
	}

	dsn := os.Getenv(envConfigPostgresDSN)
	if dsn == "" {
		return nil, fmt.Errorf("%s is required when %s is enabled", envConfigPostgresDSN, envConfigPostgresEnabled)
	}

	store, err = newPostgresConfigStoreFunc(ctx, dsn)
	if err != nil {
		return nil, err
	}

	imported, err := store.BootstrapFromFile(configFilePath())
	if err != nil {
		_ = store.Close()

		return nil, fmt.Errorf("bootstrapping postgres config store: %w", err)
	}

	if imported {
		log.Info("imported configuration into PostgreSQL config store")
	}

	syncConfigMirrorBestEffort(store, configFilePath(), "startup-sync")

	return store, nil
}

// syncConfigMirror refreshes the local YAML mirror from the active persistent
// store.  Missing data in the store is treated as first-run state and does not
// create a local file.
func syncConfigMirror(store configStore, filePath string) (err error) {
	data, err := store.Read()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("reading persistent config: %w", err)
	}

	return writeConfigMirror(filePath, data)
}

func writeConfigMirror(filePath string, data []byte) (err error) {
	err = newConfigMirrorWriter(filePath).Write(data)
	if err != nil {
		return fmt.Errorf("writing mirrored config file: %w", err)
	}

	return nil
}

func syncConfigMirrorBestEffort(store configStore, filePath string, operation string) {
	err := syncConfigMirror(store, filePath)
	if err != nil {
		log.Error("syncing local config mirror after %s: %s; continuing", operation, err)
	}
}

func writeConfigMirrorBestEffort(filePath string, data []byte, operation string) {
	err := writeConfigMirror(filePath, data)
	if err != nil {
		log.Error("writing local config mirror after %s: %s; continuing", operation, err)
	}
}

// currentConfigStore returns the active config store.  If none is explicitly
// initialized, the file-based store is used.
func currentConfigStore() (store configStore) {
	if globalContext.configStore != nil {
		return globalContext.configStore
	}

	return newFileConfigStore(configFilePath())
}

// writeConfigData writes configuration as a normal update.
func writeConfigData(data []byte) (err error) {
	return writeConfigDataWithReason(data, configChangeReasonUpdate, defaultConfigRevisionActor)
}

// writeConfigDataWithReason writes configuration to the active persistence
// backend using the provided reason metadata.
func writeConfigDataWithReason(data []byte, reason configChangeReason, actor string) (err error) {
	store := currentConfigStore()
	if reasonedStore, ok := store.(configStoreWriteWithReason); ok {
		return reasonedStore.WriteWithReason(data, reason, actor)
	}

	return store.Write(data)
}

func isPostgresConfigStoreEnabled() (ok bool, err error) {
	v := os.Getenv(envConfigPostgresEnabled)
	if v == "" {
		return false, nil
	}

	ok, err = strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("parsing %s: %w", envConfigPostgresEnabled, err)
	}

	return ok, nil
}
