//go:build integration

package home

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AdguardTeam/AdGuardHome/internal/configmigrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const defaultPostgresConfigStoreTestImage = "postgres:17-alpine"

func TestPostgresConfigStore_WriteRevision_MirrorFailure(t *testing.T) {
	storeConfigStoreGlobals(t)

	store := newTestPostgresConfigStore(t)

	firstData := testPostgresConfigStoreData("127.0.0.1:3000")
	secondData := testPostgresConfigStoreData("127.0.0.1:3001")

	require.NoError(t, store.Write(firstData))

	writer := &fakeMirrorWriter{err: assert.AnError}
	newConfigMirrorWriter = func(_ string) configMirrorWriter {
		return writer
	}

	meta, err := store.writeRevision(secondData, configChangeReasonUpdate, "tester", nil, false)
	require.NoError(t, err)
	assert.Equal(t, 1, writer.writeCount)

	active, err := store.ActiveRevision()
	require.NoError(t, err)
	assert.Equal(t, meta.RevisionID, active.RevisionID)
	assert.Equal(t, "update", active.ChangeReason)

	storedData, err := store.Read()
	require.NoError(t, err)
	assert.Equal(t, secondData, storedData)

	revisions, err := store.ListRevisions(10, 0)
	require.NoError(t, err)
	require.Len(t, revisions, 2)
	assert.Equal(t, meta.RevisionID, revisions[0].RevisionID)
}

func TestPostgresConfigStore_WriteRevisionNoop_MirrorFailure(t *testing.T) {
	storeConfigStoreGlobals(t)

	store := newTestPostgresConfigStore(t)
	configData := testPostgresConfigStoreData("127.0.0.1:3000")

	require.NoError(t, store.Write(configData))

	before, err := store.ActiveRevision()
	require.NoError(t, err)

	writer := &fakeMirrorWriter{err: assert.AnError}
	newConfigMirrorWriter = func(_ string) configMirrorWriter {
		return writer
	}

	meta, err := store.writeRevision(configData, configChangeReasonUpdate, "tester", nil, false)
	require.NoError(t, err)
	assert.Equal(t, 1, writer.writeCount)
	assert.Equal(t, before.RevisionID, meta.RevisionID)

	after, err := store.ActiveRevision()
	require.NoError(t, err)
	assert.Equal(t, before.RevisionID, after.RevisionID)

	revisions, err := store.ListRevisions(10, 0)
	require.NoError(t, err)
	assert.Len(t, revisions, 1)
}

func TestPostgresConfigStore_RollbackTo_MirrorFailure(t *testing.T) {
	storeConfigStoreGlobals(t)

	store := newTestPostgresConfigStore(t)

	firstData := testPostgresConfigStoreData("127.0.0.1:3000")
	secondData := testPostgresConfigStoreData("127.0.0.1:3001")

	require.NoError(t, store.Write(firstData))
	require.NoError(t, store.Write(secondData))

	revisions, err := store.ListRevisions(10, 0)
	require.NoError(t, err)
	require.Len(t, revisions, 2)

	latestRevisionID := revisions[0].RevisionID
	targetRevisionID := revisions[1].RevisionID

	writer := &fakeMirrorWriter{err: assert.AnError}
	newConfigMirrorWriter = func(_ string) configMirrorWriter {
		return writer
	}

	newActive, err := store.RollbackTo(targetRevisionID, "tester")
	require.NoError(t, err)
	assert.Equal(t, 1, writer.writeCount)
	require.NotNil(t, newActive.ParentRevisionID)
	require.NotNil(t, newActive.RollbackOfRevisionID)
	assert.Equal(t, latestRevisionID, *newActive.ParentRevisionID)
	assert.Equal(t, targetRevisionID, *newActive.RollbackOfRevisionID)
	assert.Equal(t, "rollback", newActive.ChangeReason)

	active, err := store.ActiveRevision()
	require.NoError(t, err)
	assert.Equal(t, newActive.RevisionID, active.RevisionID)

	storedData, err := store.Read()
	require.NoError(t, err)
	assert.Equal(t, firstData, storedData)

	revisions, err = store.ListRevisions(10, 0)
	require.NoError(t, err)
	require.Len(t, revisions, 3)
	assert.Equal(t, newActive.RevisionID, revisions[0].RevisionID)
}

func newTestPostgresConfigStore(t *testing.T) *postgresConfigStore {
	t.Helper()

	dsn := startPostgresConfigStoreContainer(t)
	dir := t.TempDir()
	globalContext.workDir = dir
	globalContext.confFilePath = filepath.Join(dir, "AdGuardHome.yaml")

	var store *postgresConfigStore
	require.Eventually(t, func() bool {
		var err error
		store, err = newPostgresConfigStore(context.Background(), dsn)

		return err == nil
	}, 30*time.Second, 500*time.Millisecond)
	require.NotNil(t, store)

	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	return store
}

func startPostgresConfigStoreContainer(t *testing.T) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	ensureDockerAvailable(t, ctx)

	image := postgresConfigStoreTestImage()
	ensureDockerImageAvailable(t, ctx, image)

	containerID := strings.TrimSpace(runDockerCommand(t, ctx,
		"run",
		"-d",
		"--rm",
		"-e", "POSTGRES_DB=nullprivate",
		"-e", "POSTGRES_USER=nullprivate",
		"-e", "POSTGRES_PASSWORD=secret",
		"-p", "127.0.0.1::5432",
		image,
	))
	require.NotEmpty(t, containerID)

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", containerID).Run()
	})

	var hostPort string
	require.Eventually(t, func() bool {
		portOut, err := runDockerCommandE(ctx, "port", containerID, "5432/tcp")
		if err != nil {
			return false
		}

		hostPort = strings.TrimSpace(portOut)
		return hostPort != ""
	}, 30*time.Second, 500*time.Millisecond)

	require.Eventually(t, func() bool {
		_, err := runDockerCommandE(ctx, "exec", containerID, "pg_isready", "-U", "nullprivate", "-d", "nullprivate")
		return err == nil
	}, 60*time.Second, time.Second)

	return fmt.Sprintf("postgres://nullprivate:secret@%s/nullprivate?sslmode=disable", hostPort)
}

func ensureDockerAvailable(t *testing.T, ctx context.Context) {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker 不可用，跳过 PostgreSQL 集成测试")
	}

	if _, err := runDockerCommandE(ctx, "info"); err != nil {
		t.Skip("docker daemon 不可用，跳过 PostgreSQL 集成测试")
	}
}

func ensureDockerImageAvailable(t *testing.T, ctx context.Context, image string) {
	t.Helper()

	if _, err := runDockerCommandE(ctx, "image", "inspect", image); err == nil {
		return
	}

	if _, err := runDockerCommandE(ctx, "pull", image); err != nil {
		t.Skipf("无法拉取测试镜像 %s，跳过 PostgreSQL 集成测试: %v", image, err)
	}
}

func runDockerCommand(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()

	out, err := runDockerCommandE(ctx, args...)
	require.NoError(t, err)

	return out
}

func runDockerCommandE(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return string(out), nil
}

func postgresConfigStoreTestImage() string {
	if image := os.Getenv("POSTGRES_CONFIG_STORE_TEST_IMAGE"); image != "" {
		return image
	}

	return defaultPostgresConfigStoreTestImage
}

func testPostgresConfigStoreData(address string) []byte {
	return []byte(fmt.Sprintf("schema_version: %d\nhttp:\n  address: %s\n", configmigrate.LastSchemaVersion, address))
}
