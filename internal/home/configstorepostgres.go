package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/AdguardTeam/AdGuardHome/internal/configmigrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	yaml "gopkg.in/yaml.v3"
)

const (
	configRevisionsTableName      = "config_revisions"
	configHeadTableName           = "config_head"
	legacyConfigSectionsTableName = "config_sections"
	fullConfigSectionName         = "__full_config__"
	defaultConfigHeadKey          = "default"
	configStoreAdvisoryLockKey    = int64(0x4e505f4346475354)
)

const createConfigRevisionsTableQuery = `
CREATE TABLE IF NOT EXISTS config_revisions (
	revision_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	parent_revision_id bigint NULL REFERENCES config_revisions(revision_id) ON DELETE SET NULL,
	rollback_of_revision_id bigint NULL REFERENCES config_revisions(revision_id) ON DELETE SET NULL,
	schema_version integer NOT NULL,
	yaml_body text NOT NULL,
	yaml_sha256 text NOT NULL,
	change_reason text NOT NULL CHECK (change_reason IN ('bootstrap', 'update', 'migrate', 'rollback', 'legacy_import')),
	created_by text NOT NULL DEFAULT 'system',
	created_at timestamptz NOT NULL DEFAULT now()
)`

const createConfigHeadTableQuery = `
CREATE TABLE IF NOT EXISTS config_head (
	head_key text PRIMARY KEY,
	active_revision_id bigint NOT NULL REFERENCES config_revisions(revision_id),
	updated_at timestamptz NOT NULL DEFAULT now()
)`

const createConfigRevisionsCreatedAtIndexQuery = `
CREATE INDEX IF NOT EXISTS config_revisions_created_at_idx
ON config_revisions (created_at DESC, revision_id DESC)`

const createConfigRevisionsParentIndexQuery = `
CREATE INDEX IF NOT EXISTS config_revisions_parent_idx
ON config_revisions (parent_revision_id)`

const createConfigRevisionsRollbackIndexQuery = `
CREATE INDEX IF NOT EXISTS config_revisions_rollback_idx
ON config_revisions (rollback_of_revision_id)`

const createConfigRevisionsHashIndexQuery = `
CREATE INDEX IF NOT EXISTS config_revisions_sha_idx
ON config_revisions (yaml_sha256)`

const acquireConfigStoreAdvisoryLockQuery = `SELECT pg_advisory_xact_lock($1)`

const activeRevisionExistsQuery = `
SELECT EXISTS (SELECT 1 FROM config_head WHERE head_key = $1)`

const selectActiveRevisionQuery = `
SELECT r.yaml_body,
	r.revision_id,
	r.parent_revision_id,
	r.rollback_of_revision_id,
	r.schema_version,
	r.yaml_sha256,
	r.change_reason,
	r.created_by,
	r.created_at
FROM config_head AS h
JOIN config_revisions AS r ON r.revision_id = h.active_revision_id
WHERE h.head_key = $1`

const selectRevisionByIDQuery = `
SELECT yaml_body,
	revision_id,
	parent_revision_id,
	rollback_of_revision_id,
	schema_version,
	yaml_sha256,
	change_reason,
	created_by,
	created_at
FROM config_revisions
WHERE revision_id = $1`

const listRevisionsQuery = `
SELECT revision_id,
	parent_revision_id,
	rollback_of_revision_id,
	schema_version,
	yaml_sha256,
	change_reason,
	created_by,
	created_at
FROM config_revisions
WHERE ($1 <= 0 OR revision_id < $1)
ORDER BY revision_id DESC
LIMIT $2`

const insertRevisionQuery = `
INSERT INTO config_revisions (
	parent_revision_id,
	rollback_of_revision_id,
	schema_version,
	yaml_body,
	yaml_sha256,
	change_reason,
	created_by
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING revision_id, created_at`

const upsertHeadQuery = `
INSERT INTO config_head (head_key, active_revision_id, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (head_key)
DO UPDATE SET active_revision_id = EXCLUDED.active_revision_id, updated_at = now()`

const pruneOldRevisionsQuery = `
WITH keep AS (
	SELECT revision_id
	FROM config_revisions
	ORDER BY revision_id DESC
	LIMIT $1
)
DELETE FROM config_revisions
WHERE revision_id NOT IN (SELECT revision_id FROM keep)
	AND revision_id <> (
		SELECT active_revision_id
		FROM config_head
		WHERE head_key = $2
	)`

const legacyConfigTableExistsQuery = `
SELECT EXISTS (
	SELECT 1
	FROM information_schema.tables
	WHERE table_schema = current_schema()
		AND table_name = $1
)`

const readLegacyConfigSectionsQuery = `
SELECT section_name, section_order, yaml_body
FROM config_sections
ORDER BY section_order`

type postgresConfigStore struct {
	pool *pgxpool.Pool
}

type configSection struct {
	name  string
	order int
	body  string
}

type pgQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// type checks
var _ configStore = (*postgresConfigStore)(nil)
var _ configStoreWriteWithReason = (*postgresConfigStore)(nil)
var _ versionedConfigStore = (*postgresConfigStore)(nil)

func newPostgresConfigStore(parentCtx context.Context, dsn string) (store *postgresConfigStore, err error) {
	ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres config store: %w", err)
	}

	store = &postgresConfigStore{pool: pool}

	err = store.pool.Ping(ctx)
	if err != nil {
		_ = store.Close()

		return nil, fmt.Errorf("pinging postgres config store: %w", err)
	}

	err = store.ensureSchema(ctx)
	if err != nil {
		_ = store.Close()

		return nil, err
	}

	return store, nil
}

func (s *postgresConfigStore) ensureSchema(ctx context.Context) (err error) {
	queries := []string{
		createConfigRevisionsTableQuery,
		createConfigHeadTableQuery,
		createConfigRevisionsCreatedAtIndexQuery,
		createConfigRevisionsParentIndexQuery,
		createConfigRevisionsRollbackIndexQuery,
		createConfigRevisionsHashIndexQuery,
	}

	for _, q := range queries {
		_, err = s.pool.Exec(ctx, q)
		if err != nil {
			return fmt.Errorf("ensuring postgres config store schema: %w", err)
		}
	}

	return nil
}

func (s *postgresConfigStore) Exists() (ok bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return s.headExists(ctx, s.pool)
}

func (s *postgresConfigStore) Read() (data []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, _, err = s.readActiveRevision(ctx, s.pool)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (s *postgresConfigStore) Write(data []byte) (err error) {
	_, err = s.writeRevision(data, configChangeReasonUpdate, defaultConfigRevisionActor, nil, false)

	return err
}

func (s *postgresConfigStore) WriteWithReason(data []byte, reason configChangeReason, actor string) (err error) {
	_, err = s.writeRevision(data, reason, actor, nil, false)

	return err
}

func (s *postgresConfigStore) ActiveRevision() (meta ConfigRevisionMeta, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, meta, err = s.readActiveRevision(ctx, s.pool)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	return meta, nil
}

func (s *postgresConfigStore) ListRevisions(limit int, beforeRevisionID int64) (items []ConfigRevisionMeta, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if limit <= 0 {
		limit = configRevisionRetentionLimit
	}

	rows, err := s.pool.Query(ctx, listRevisionsQuery, beforeRevisionID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing config revisions: %w", err)
	}
	defer rows.Close()

	items = make([]ConfigRevisionMeta, 0, limit)
	for rows.Next() {
		meta, scanErr := scanRevisionMeta(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scanning config revision: %w", scanErr)
		}

		items = append(items, meta)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading config revisions: %w", err)
	}

	return items, nil
}

func (s *postgresConfigStore) ReadRevision(revisionID int64) (data []byte, meta ConfigRevisionMeta, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return s.readRevision(ctx, s.pool, revisionID)
}

func (s *postgresConfigStore) RollbackTo(revisionID int64, actor string) (newActive ConfigRevisionMeta, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := s.beginLockedTx(ctx)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, tx.Rollback(ctx))
		}
	}()

	targetData, targetMeta, err := s.readRevision(ctx, tx, revisionID)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	if targetMeta.SchemaVersion > configmigrate.LastSchemaVersion {
		return ConfigRevisionMeta{}, fmt.Errorf(
			"rollback target schema version %d is newer than supported %d",
			targetMeta.SchemaVersion,
			configmigrate.LastSchemaVersion,
		)
	}

	rollbackData := targetData
	if targetMeta.SchemaVersion < configmigrate.LastSchemaVersion {
		migrator := configmigrate.New(&configmigrate.Config{
			WorkingDir: globalContext.workDir,
			DataDir:    globalContext.getDataDir(),
		})

		rollbackData, _, err = migrator.Migrate(rollbackData, configmigrate.LastSchemaVersion)
		if err != nil {
			return ConfigRevisionMeta{}, fmt.Errorf("migrating rollback config: %w", err)
		}
	}

	_, activeMeta, err := s.readActiveRevision(ctx, tx)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	newActive, err = s.insertRevisionTx(
		ctx,
		tx,
		rollbackData,
		configChangeReasonRollback,
		normalizeConfigActor(actor),
		ptrToInt64(activeMeta.RevisionID),
		ptrToInt64(targetMeta.RevisionID),
	)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	newActive.ParentRevisionID = ptrToInt64(activeMeta.RevisionID)

	err = s.setActiveRevisionTx(ctx, tx, newActive.RevisionID)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	err = s.pruneOldRevisionsTx(ctx, tx, configRevisionRetentionLimit)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	err = tx.Commit(ctx)
	if err != nil {
		return ConfigRevisionMeta{}, fmt.Errorf("committing rollback revision: %w", err)
	}

	committed = true

	writeConfigMirrorBestEffort(configFilePath(), rollbackData, string(configChangeReasonRollback))

	return newActive, nil
}

func (s *postgresConfigStore) BootstrapFromFile(filePath string) (imported bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := s.beginLockedTx(ctx)
	if err != nil {
		return false, err
	}

	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, tx.Rollback(ctx))
		}
	}()

	exists, err := s.headExists(ctx, tx)
	if err != nil {
		return false, err
	}

	if exists {
		return false, nil
	}

	imported, err = s.bootstrapLegacyConfigTx(ctx, tx)
	if err != nil {
		return false, err
	}
	if imported {
		err = tx.Commit(ctx)
		if err != nil {
			return false, fmt.Errorf("committing legacy config import: %w", err)
		}

		committed = true

		return true, nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, fmt.Errorf("reading config bootstrap file: %w", err)
	}

	meta, err := s.insertRevisionTx(
		ctx,
		tx,
		data,
		configChangeReasonBootstrap,
		defaultConfigRevisionActor,
		nil,
		nil,
	)
	if err != nil {
		return false, fmt.Errorf("writing bootstrap config to postgres: %w", err)
	}

	err = s.setActiveRevisionTx(ctx, tx, meta.RevisionID)
	if err != nil {
		return false, err
	}

	err = tx.Commit(ctx)
	if err != nil {
		return false, fmt.Errorf("committing bootstrap config import: %w", err)
	}

	committed = true

	return true, nil
}

func (s *postgresConfigStore) Close() (err error) {
	if s.pool != nil {
		s.pool.Close()
	}

	return nil
}

func (s *postgresConfigStore) writeRevision(
	data []byte,
	reason configChangeReason,
	actor string,
	rollbackOfRevisionID *int64,
	forceCreate bool,
) (meta ConfigRevisionMeta, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := s.beginLockedTx(ctx)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, tx.Rollback(ctx))
		}
	}()

	activeData, activeMeta, err := s.readActiveRevision(ctx, tx)
	if err != nil && !os.IsNotExist(err) {
		return ConfigRevisionMeta{}, err
	}

	if err == nil && !forceCreate {
		hash := hashConfigData(data)
		if activeMeta.SHA256 == hash && bytes.Equal(activeData, data) {
			err = tx.Commit(ctx)
			if err != nil {
				return ConfigRevisionMeta{}, fmt.Errorf("committing noop config write: %w", err)
			}

			committed = true

			writeConfigMirrorBestEffort(configFilePath(), data, "noop-refresh")

			return activeMeta, nil
		}
	}

	var parentRevisionID *int64
	if err == nil {
		parentRevisionID = ptrToInt64(activeMeta.RevisionID)
	}

	meta, err = s.insertRevisionTx(
		ctx,
		tx,
		data,
		normalizeConfigReason(reason),
		normalizeConfigActor(actor),
		parentRevisionID,
		rollbackOfRevisionID,
	)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	err = s.setActiveRevisionTx(ctx, tx, meta.RevisionID)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	err = s.pruneOldRevisionsTx(ctx, tx, configRevisionRetentionLimit)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	err = tx.Commit(ctx)
	if err != nil {
		return ConfigRevisionMeta{}, fmt.Errorf("committing config revision: %w", err)
	}

	committed = true

	writeConfigMirrorBestEffort(configFilePath(), data, string(normalizeConfigReason(reason)))

	return meta, nil
}

func (s *postgresConfigStore) beginLockedTx(ctx context.Context) (tx pgx.Tx, err error) {
	tx, err = s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting config store transaction: %w", err)
	}

	_, err = tx.Exec(ctx, acquireConfigStoreAdvisoryLockQuery, configStoreAdvisoryLockKey)
	if err != nil {
		_ = tx.Rollback(ctx)

		return nil, fmt.Errorf("acquiring config store advisory lock: %w", err)
	}

	return tx, nil
}

func (s *postgresConfigStore) headExists(ctx context.Context, q pgQueryer) (ok bool, err error) {
	err = q.QueryRow(ctx, activeRevisionExistsQuery, defaultConfigHeadKey).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("checking postgres config store contents: %w", err)
	}

	return ok, nil
}

func (s *postgresConfigStore) readActiveRevision(ctx context.Context, q pgQueryer) (data []byte, meta ConfigRevisionMeta, err error) {
	return s.scanRevisionWithData(
		q.QueryRow(ctx, selectActiveRevisionQuery, defaultConfigHeadKey),
		"reading active config revision",
	)
}

func (s *postgresConfigStore) readRevision(
	ctx context.Context,
	q pgQueryer,
	revisionID int64,
) (data []byte, meta ConfigRevisionMeta, err error) {
	return s.scanRevisionWithData(
		q.QueryRow(ctx, selectRevisionByIDQuery, revisionID),
		"reading config revision",
	)
}

func (s *postgresConfigStore) scanRevisionWithData(
	row pgx.Row,
	action string,
) (data []byte, meta ConfigRevisionMeta, err error) {
	var (
		parentRevisionID     sql.NullInt64
		rollbackOfRevisionID sql.NullInt64
		schemaVersion        int
	)

	err = row.Scan(
		&data,
		&meta.RevisionID,
		&parentRevisionID,
		&rollbackOfRevisionID,
		&schemaVersion,
		&meta.SHA256,
		&meta.ChangeReason,
		&meta.CreatedBy,
		&meta.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ConfigRevisionMeta{}, os.ErrNotExist
		}

		return nil, ConfigRevisionMeta{}, fmt.Errorf("%s: %w", action, err)
	}

	meta.SchemaVersion = uint(schemaVersion)
	meta.ParentRevisionID = nullableInt64Ptr(parentRevisionID)
	meta.RollbackOfRevisionID = nullableInt64Ptr(rollbackOfRevisionID)

	return data, meta, nil
}

func scanRevisionMeta(row interface{ Scan(dest ...any) error }) (meta ConfigRevisionMeta, err error) {
	var (
		parentRevisionID     sql.NullInt64
		rollbackOfRevisionID sql.NullInt64
		schemaVersion        int
	)

	err = row.Scan(
		&meta.RevisionID,
		&parentRevisionID,
		&rollbackOfRevisionID,
		&schemaVersion,
		&meta.SHA256,
		&meta.ChangeReason,
		&meta.CreatedBy,
		&meta.CreatedAt,
	)
	if err != nil {
		return ConfigRevisionMeta{}, err
	}

	meta.SchemaVersion = uint(schemaVersion)
	meta.ParentRevisionID = nullableInt64Ptr(parentRevisionID)
	meta.RollbackOfRevisionID = nullableInt64Ptr(rollbackOfRevisionID)

	return meta, nil
}

func (s *postgresConfigStore) insertRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	data []byte,
	reason configChangeReason,
	actor string,
	parentRevisionID *int64,
	rollbackOfRevisionID *int64,
) (meta ConfigRevisionMeta, err error) {
	schemaVersion, err := extractConfigSchemaVersion(data)
	if err != nil {
		return ConfigRevisionMeta{}, fmt.Errorf("extracting config schema version: %w", err)
	}

	hash := hashConfigData(data)

	err = tx.QueryRow(
		ctx,
		insertRevisionQuery,
		parentRevisionID,
		rollbackOfRevisionID,
		int(schemaVersion),
		string(data),
		hash,
		string(reason),
		actor,
	).Scan(&meta.RevisionID, &meta.CreatedAt)
	if err != nil {
		return ConfigRevisionMeta{}, fmt.Errorf("inserting config revision: %w", err)
	}

	meta.ParentRevisionID = cloneInt64Ptr(parentRevisionID)
	meta.RollbackOfRevisionID = cloneInt64Ptr(rollbackOfRevisionID)
	meta.SchemaVersion = schemaVersion
	meta.SHA256 = hash
	meta.ChangeReason = string(reason)
	meta.CreatedBy = actor

	return meta, nil
}

func (s *postgresConfigStore) setActiveRevisionTx(ctx context.Context, tx pgx.Tx, revisionID int64) (err error) {
	_, err = tx.Exec(ctx, upsertHeadQuery, defaultConfigHeadKey, revisionID)
	if err != nil {
		return fmt.Errorf("updating active config revision: %w", err)
	}

	return nil
}

func (s *postgresConfigStore) pruneOldRevisionsTx(ctx context.Context, tx pgx.Tx, limit int) (err error) {
	if limit <= 0 {
		return nil
	}

	_, err = tx.Exec(ctx, pruneOldRevisionsQuery, limit, defaultConfigHeadKey)
	if err != nil {
		return fmt.Errorf("pruning old config revisions: %w", err)
	}

	return nil
}

func (s *postgresConfigStore) bootstrapLegacyConfigTx(ctx context.Context, tx pgx.Tx) (imported bool, err error) {
	exists, err := s.legacyConfigTableExists(ctx, tx)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}

	sections, err := s.readLegacyConfigSections(ctx, tx)
	if err != nil {
		return false, err
	}
	if len(sections) == 0 {
		return false, nil
	}

	data, err := assembleConfigSections(sections)
	if err != nil {
		return false, fmt.Errorf("assembling legacy config sections: %w", err)
	}

	meta, err := s.insertRevisionTx(
		ctx,
		tx,
		data,
		configChangeReasonLegacyImport,
		defaultConfigRevisionActor,
		nil,
		nil,
	)
	if err != nil {
		return false, fmt.Errorf("importing legacy config revisions: %w", err)
	}

	err = s.setActiveRevisionTx(ctx, tx, meta.RevisionID)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (s *postgresConfigStore) legacyConfigTableExists(ctx context.Context, q pgQueryer) (ok bool, err error) {
	err = q.QueryRow(ctx, legacyConfigTableExistsQuery, legacyConfigSectionsTableName).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("checking legacy config store table: %w", err)
	}

	return ok, nil
}

func (s *postgresConfigStore) readLegacyConfigSections(ctx context.Context, q pgQueryer) (sections []configSection, err error) {
	rows, err := q.Query(ctx, readLegacyConfigSectionsQuery)
	if err != nil {
		return nil, fmt.Errorf("querying legacy config sections: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sec configSection
		err = rows.Scan(&sec.name, &sec.order, &sec.body)
		if err != nil {
			return nil, fmt.Errorf("scanning legacy config section: %w", err)
		}

		sections = append(sections, sec)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading legacy config sections: %w", err)
	}

	return sections, nil
}

func splitConfigSections(data []byte) (sections []configSection, err error) {
	_, err = decodeYAMLDocument(data)
	if err != nil {
		return nil, err
	}

	return []configSection{{
		name:  fullConfigSectionName,
		order: 0,
		body:  string(data),
	}}, nil
}

func assembleConfigSections(sections []configSection) (data []byte, err error) {
	if len(sections) == 0 {
		return nil, os.ErrNotExist
	}

	if len(sections) == 1 && sections[0].name == fullConfigSectionName {
		_, err = decodeYAMLDocument([]byte(sections[0].body))
		if err != nil {
			return nil, fmt.Errorf("decoding full config document: %w", err)
		}

		return []byte(sections[0].body), nil
	}

	slices.SortFunc(sections, func(a, b configSection) int {
		return a.order - b.order
	})

	root := &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
	}

	seen := map[string]struct{}{}
	for _, sec := range sections {
		if _, ok := seen[sec.name]; ok {
			return nil, fmt.Errorf("duplicate config section: %q", sec.name)
		}
		seen[sec.name] = struct{}{}

		val, decodeErr := decodeYAMLValue([]byte(sec.body))
		if decodeErr != nil {
			return nil, fmt.Errorf("decoding config section %q: %w", sec.name, decodeErr)
		}

		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: sec.name},
			val,
		)
	}

	return encodeYAMLNode(&yaml.Node{
		Kind:    yaml.DocumentNode,
		Content: []*yaml.Node{root},
	})
}

func decodeYAMLDocument(data []byte) (doc *yaml.Node, err error) {
	doc = &yaml.Node{}
	err = yaml.Unmarshal(data, doc)
	if err != nil {
		return nil, fmt.Errorf("decoding yaml: %w", err)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("unexpected yaml document structure")
	}

	return doc, nil
}

func decodeYAMLValue(data []byte) (node *yaml.Node, err error) {
	doc, err := decodeYAMLDocument(data)
	if err != nil {
		return nil, err
	}

	return doc.Content[0], nil
}

func encodeYAMLNode(node *yaml.Node) (data []byte, err error) {
	buf := &bytes.Buffer{}
	enc := yaml.NewEncoder(buf)
	enc.SetIndent(2)

	err = enc.Encode(node)
	if err != nil {
		return nil, err
	}

	err = enc.Close()
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func extractConfigSchemaVersion(data []byte) (ver uint, err error) {
	_, err = decodeYAMLDocument(data)
	if err != nil {
		return 0, err
	}

	conf := struct {
		SchemaVersion uint `yaml:"schema_version"`
	}{}

	err = yaml.Unmarshal(data, &conf)
	if err != nil {
		return 0, fmt.Errorf("decoding schema version: %w", err)
	}

	return conf.SchemaVersion, nil
}

func hashConfigData(data []byte) (sum string) {
	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:])
}

func normalizeConfigReason(reason configChangeReason) (normalized configChangeReason) {
	if reason == "" {
		return configChangeReasonUpdate
	}

	return reason
}

func normalizeConfigActor(actor string) (normalized string) {
	if actor == "" {
		return defaultConfigRevisionActor
	}

	return actor
}

func nullableInt64Ptr(v sql.NullInt64) (ptr *int64) {
	if !v.Valid {
		return nil
	}

	return ptrToInt64(v.Int64)
}

func ptrToInt64(v int64) (ptr *int64) {
	ptr = new(int64)
	*ptr = v

	return ptr
}

func cloneInt64Ptr(src *int64) (dst *int64) {
	if src == nil {
		return nil
	}

	return ptrToInt64(*src)
}
