package migrations

import "embed"

//go:embed migrations
var DBmigrations embed.FS

//go:embed backup
var BackupMigrations embed.FS
