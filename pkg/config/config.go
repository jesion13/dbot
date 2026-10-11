package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fr-str/env"
)

type Dirkey string

const (
	DirKey Dirkey = "tmp_dir"
)

var (
	// canceled once config has loaded, unblocking every <-config.Ctx.Done()
	Ctx, cancel = context.WithCancel(context.Background())

	ENV       string
	LOG_LEVEL string
	DB_DIR    string

	TOKEN    string
	GUILD_ID string

	TMP_PATH string

	COOKIE_PATH string

	FFMPEG_HW_ACCEL bool

	BACKUP_DIR string
)

func Load() {
	defer cancel()
	ENV = env.Get("ENV", "dev")
	LOG_LEVEL = env.Get("LOG_LEVEL", "debug")
	DB_DIR = env.Get("DB_DIR", "data")
	TOKEN = env.Get("TOKEN", "")
	GUILD_ID = env.Get("GUILD_ID", "")
	TMP_PATH = env.Get("TMP_PATH", filepath.Join(os.TempDir(), "dbot"))
	COOKIE_PATH = env.Get("COOKIE_PATH", "")
	FFMPEG_HW_ACCEL = env.Get("FFMPEG_HW_ACCEL", false)
	BACKUP_DIR = env.Get("BACKUP_DIR", filepath.Join(os.TempDir(), "dbot", "backup"))

	fmt.Println("ENV =", ENV)
	fmt.Println("LOG_LEVEL =", LOG_LEVEL)
	fmt.Println("DB_DIR =", DB_DIR)
	fmt.Println("GUILD_ID =", GUILD_ID)
	fmt.Println("TMP_PATH =", TMP_PATH)
	fmt.Println("COOKIE_PATH =", COOKIE_PATH)
	fmt.Println("FFMPEG_HW_ACCEL =", FFMPEG_HW_ACCEL)
	fmt.Println("BACKUP_DIR =", BACKUP_DIR)

	dirs := []string{
		BACKUP_DIR,
		DB_DIR,
	}

	for _, dir := range dirs {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			continue
		}

		err := os.MkdirAll(dir, 0o777)
		if err != nil {
			panic(err)
		}
	}
}
