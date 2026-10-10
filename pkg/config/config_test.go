package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("DB_DIR", filepath.Join(tmp, "db"))
		t.Setenv("BACKUP_DIR", filepath.Join(tmp, "backup"))
		t.Setenv("TMP_PATH", filepath.Join(tmp, "tmp"))

		Load()

		assert.Equal(t, "dev", ENV)
		assert.Equal(t, "debug", LOG_LEVEL)
		assert.False(t, FFMPEG_HW_ACCEL)
	})

	t.Run("creates missing dirs", func(t *testing.T) {
		tmp := t.TempDir()
		dbDir := filepath.Join(tmp, "db")
		backupDir := filepath.Join(tmp, "backup")
		t.Setenv("DB_DIR", dbDir)
		t.Setenv("BACKUP_DIR", backupDir)
		t.Setenv("TMP_PATH", filepath.Join(tmp, "tmp"))

		Load()

		_, err := os.Stat(dbDir)
		require.NoError(t, err)
		_, err = os.Stat(backupDir)
		require.NoError(t, err)
	})

	t.Run("parses FFMPEG_HW_ACCEL", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("DB_DIR", filepath.Join(tmp, "db"))
		t.Setenv("BACKUP_DIR", filepath.Join(tmp, "backup"))
		t.Setenv("TMP_PATH", filepath.Join(tmp, "tmp"))
		t.Setenv("FFMPEG_HW_ACCEL", "true")

		Load()

		assert.True(t, FFMPEG_HW_ACCEL)
	})
}
