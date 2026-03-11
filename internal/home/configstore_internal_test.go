package home

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AdguardTeam/golibs/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"
)

type fakePlainConfigStore struct {
	exists  bool
	data    []byte
	err     error
	readErr error
}

func (s *fakePlainConfigStore) Exists() (ok bool, err error) {
	if s.err != nil {
		return false, s.err
	}

	return s.exists, nil
}

func (s *fakePlainConfigStore) Read() (data []byte, err error) {
	if s.readErr != nil {
		return nil, s.readErr
	}

	if s.err != nil {
		return nil, s.err
	}

	return s.data, nil
}

func (s *fakePlainConfigStore) Write(data []byte) (err error) {
	if s.err != nil {
		return s.err
	}

	s.exists = true
	s.data = append(s.data[:0], data...)

	return nil
}

func (s *fakePlainConfigStore) BootstrapFromFile(_ string) (imported bool, err error) {
	return false, s.err
}

func (s *fakePlainConfigStore) Close() (err error) {
	return nil
}

type fakeConfigStore struct {
	exists            bool
	data              []byte
	err               error
	readErr           error
	bootstrapErr      error
	bootstrapImported bool
	closeErr          error
	closeCount        int
	writeCount        int
	writeReason       configChangeReason
	writeActor        string
}

func (s *fakeConfigStore) Exists() (ok bool, err error) {
	if s.err != nil {
		return false, s.err
	}

	return s.exists, nil
}

func (s *fakeConfigStore) Read() (data []byte, err error) {
	if s.readErr != nil {
		return nil, s.readErr
	}

	if s.err != nil {
		return nil, s.err
	}

	return s.data, nil
}

func (s *fakeConfigStore) Write(data []byte) (err error) {
	s.writeCount++
	if s.err != nil {
		return s.err
	}

	s.exists = true
	s.data = append(s.data[:0], data...)

	return nil
}

func (s *fakeConfigStore) WriteWithReason(data []byte, reason configChangeReason, actor string) (err error) {
	s.writeReason = reason
	s.writeActor = actor

	return s.Write(data)
}

func (s *fakeConfigStore) BootstrapFromFile(_ string) (imported bool, err error) {
	if s.bootstrapErr != nil {
		return false, s.bootstrapErr
	}

	if s.err != nil {
		return false, s.err
	}

	return s.bootstrapImported, nil
}

func (s *fakeConfigStore) Close() (err error) {
	s.closeCount++

	return s.closeErr
}

type fakeMirrorWriter struct {
	err        error
	writeCount int
	data       []byte
}

func (w *fakeMirrorWriter) Write(data []byte) (err error) {
	w.writeCount++
	w.data = append(w.data[:0], data...)

	return w.err
}

func storeConfigStoreGlobals(tb testing.TB) {
	tb.Helper()

	prevStore := globalContext.configStore
	prevConfPath := globalContext.confFilePath
	prevWorkDir := globalContext.workDir
	prevFileData := config.fileData
	prevMirrorWriter := newConfigMirrorWriter
	prevPostgresFactory := newPostgresConfigStoreFunc

	tb.Cleanup(func() {
		globalContext.configStore = prevStore
		globalContext.confFilePath = prevConfPath
		globalContext.workDir = prevWorkDir
		config.fileData = prevFileData
		newConfigMirrorWriter = prevMirrorWriter
		newPostgresConfigStoreFunc = prevPostgresFactory
	})
}

func TestSplitConfigSectionsRoundTrip(t *testing.T) {
	input := []byte(`http:
  address: 127.0.0.1:3000
dns:
  bind_hosts:
    - 0.0.0.0
filters:
  - enabled: true
    url: https://example.org/filter.txt
language: zh-cn
theme: auto
schema_version: 29
ruleset: null
`)

	sections, err := splitConfigSections(input)
	require.NoError(t, err)
	require.Len(t, sections, 1)
	assert.Equal(t, fullConfigSectionName, sections[0].name)

	out, err := assembleConfigSections(sections)
	require.NoError(t, err)
	assert.Equal(t, input, out)

	var want map[string]any
	require.NoError(t, yaml.Unmarshal(input, &want))

	var got map[string]any
	require.NoError(t, yaml.Unmarshal(out, &got))

	assert.Equal(t, want, got)
}

func TestSplitConfigSectionsRejectsInvalidYAML(t *testing.T) {
	_, err := splitConfigSections([]byte("http: [1\n"))
	require.Error(t, err)
	assert.ErrorContains(t, err, "decoding yaml")
}

func TestAssembleConfigSectionsRejectsDuplicate(t *testing.T) {
	_, err := assembleConfigSections([]configSection{{name: "http", order: 0, body: "1\n"}, {name: "http", order: 1, body: "2\n"}})
	require.Error(t, err)
	assert.ErrorContains(t, err, `duplicate config section: "http"`)
}

func TestSplitConfigSectionsKeepsCrossSectionAnchors(t *testing.T) {
	input := []byte(`http: &shared
  address: 127.0.0.1:3000
dns:
  bind_hosts:
    - *shared
`)

	sections, err := splitConfigSections(input)
	require.NoError(t, err)

	out, err := assembleConfigSections(sections)
	require.NoError(t, err)

	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal(out, &doc))
}

func TestExtractConfigSchemaVersion(t *testing.T) {
	ver, err := extractConfigSchemaVersion([]byte("schema_version: 29\nhttp:\n  address: 127.0.0.1:3000\n"))
	require.NoError(t, err)
	assert.EqualValues(t, 29, ver)
}

func TestReadLogSettingsUsesConfigStore(t *testing.T) {
	storeConfigStoreGlobals(t)

	config.fileData = nil
	globalContext.configStore = &fakeConfigStore{
		exists: true,
		data:   []byte("log:\n  enabled: false\n  verbose: true\n"),
	}

	ls := readLogSettings()
	require.NotNil(t, ls)
	assert.False(t, ls.Enabled)
	assert.True(t, ls.Verbose)
}

func TestDetectFirstRunUsesConfigStore(t *testing.T) {
	t.Run("existing config", func(t *testing.T) {
		storeConfigStoreGlobals(t)
		globalContext.configStore = &fakeConfigStore{exists: true}

		assert.False(t, detectFirstRun())
	})

	t.Run("missing config", func(t *testing.T) {
		storeConfigStoreGlobals(t)
		globalContext.configStore = &fakeConfigStore{exists: false}

		assert.True(t, detectFirstRun())
	})

	t.Run("store error", func(t *testing.T) {
		storeConfigStoreGlobals(t)
		globalContext.configStore = &fakeConfigStore{err: errors.Error("boom")}

		assert.True(t, detectFirstRun())
	})
}

func TestWriteConfigDataWithReasonUsesReasonedStore(t *testing.T) {
	storeConfigStoreGlobals(t)

	store := &fakeConfigStore{}
	globalContext.configStore = store

	data := []byte("schema_version: 29\n")
	err := writeConfigDataWithReason(data, configChangeReasonMigrate, "tester")
	require.NoError(t, err)

	assert.Equal(t, 1, store.writeCount)
	assert.Equal(t, configChangeReasonMigrate, store.writeReason)
	assert.Equal(t, "tester", store.writeActor)
	assert.Equal(t, data, store.data)
}

func TestNewConfigStoreMissingDSN(t *testing.T) {
	t.Setenv(envConfigPostgresEnabled, "true")
	t.Setenv(envConfigPostgresDSN, "")

	_, err := newConfigStore(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, envConfigPostgresDSN)
}

func TestNewConfigStoreInvalidEnabledEnv(t *testing.T) {
	t.Setenv(envConfigPostgresEnabled, "wat")

	_, err := newConfigStore(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, envConfigPostgresEnabled)
}

func TestNewPostgresConfigStoreInvalidDSN(t *testing.T) {
	_, err := newPostgresConfigStore(context.Background(), "postgres://localhost:abc/test")
	require.Error(t, err)
}

func TestSyncConfigMirror(t *testing.T) {
	t.Run("writes local mirror from store", func(t *testing.T) {
		dir := t.TempDir()
		confPath := filepath.Join(dir, "AdGuardHome.yaml")
		data := []byte("http:\n  address: 127.0.0.1:3000\n")

		err := syncConfigMirror(&fakeConfigStore{exists: true, data: data}, confPath)
		require.NoError(t, err)

		fileData, err := os.ReadFile(confPath)
		require.NoError(t, err)
		assert.Equal(t, data, fileData)
	})

	t.Run("ignores empty store", func(t *testing.T) {
		dir := t.TempDir()
		confPath := filepath.Join(dir, "AdGuardHome.yaml")

		err := syncConfigMirror(&fakeConfigStore{readErr: os.ErrNotExist}, confPath)
		require.NoError(t, err)

		_, err = os.Stat(confPath)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("returns mirror write errors", func(t *testing.T) {
		storeConfigStoreGlobals(t)

		writer := &fakeMirrorWriter{err: errors.Error("mirror boom")}
		newConfigMirrorWriter = func(_ string) configMirrorWriter {
			return writer
		}

		err := syncConfigMirror(&fakeConfigStore{exists: true, data: []byte("http:\n  address: 127.0.0.1:3000\n")}, "AdGuardHome.yaml")
		require.Error(t, err)
		assert.ErrorContains(t, err, "writing mirrored config file")
		assert.Equal(t, 1, writer.writeCount)
	})

	t.Run("best effort ignores mirror write errors", func(t *testing.T) {
		storeConfigStoreGlobals(t)

		writer := &fakeMirrorWriter{err: errors.Error("mirror boom")}
		newConfigMirrorWriter = func(_ string) configMirrorWriter {
			return writer
		}

		syncConfigMirrorBestEffort(
			&fakeConfigStore{exists: true, data: []byte("http:\n  address: 127.0.0.1:3000\n")},
			"AdGuardHome.yaml",
			"startup-sync",
		)

		assert.Equal(t, 1, writer.writeCount)
	})
}

func TestWriteConfigMirrorBestEffort(t *testing.T) {
	storeConfigStoreGlobals(t)

	writer := &fakeMirrorWriter{err: errors.Error("mirror boom")}
	newConfigMirrorWriter = func(_ string) configMirrorWriter {
		return writer
	}

	writeConfigMirrorBestEffort("AdGuardHome.yaml", []byte("schema_version: 29\n"), "update")

	assert.Equal(t, 1, writer.writeCount)
	assert.Equal(t, []byte("schema_version: 29\n"), writer.data)
}

func TestNewConfigStoreIgnoresMirrorWriteErrors(t *testing.T) {
	storeConfigStoreGlobals(t)

	t.Setenv(envConfigPostgresEnabled, "true")
	t.Setenv(envConfigPostgresDSN, "postgres://example/nullprivate")

	store := &fakeConfigStore{
		exists: true,
		data:   []byte("schema_version: 29\nhttp:\n  address: 127.0.0.1:3000\n"),
	}
	newPostgresConfigStoreFunc = func(_ context.Context, dsn string) (configStore, error) {
		assert.Equal(t, "postgres://example/nullprivate", dsn)

		return store, nil
	}

	writer := &fakeMirrorWriter{err: errors.Error("mirror boom")}
	newConfigMirrorWriter = func(_ string) configMirrorWriter {
		return writer
	}

	got, err := newConfigStore(context.Background())
	require.NoError(t, err)
	assert.Same(t, store, got)
	assert.Equal(t, 1, writer.writeCount)
	assert.Equal(t, 0, store.closeCount)
}

func TestWriteConfigDataUsesFallbackStore(t *testing.T) {
	storeConfigStoreGlobals(t)

	store := &fakePlainConfigStore{}
	globalContext.configStore = store

	data := []byte("http:\n  address: 127.0.0.1:3000\n")
	err := writeConfigData(data)
	require.NoError(t, err)

	stored, err := store.Read()
	require.NoError(t, err)
	assert.Equal(t, data, stored)
}
