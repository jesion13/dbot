package dbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/webp"

	"github.com/corona10/goimagehash"

	"dbot/pkg/backup"
	"dbot/pkg/config"

	"github.com/disgoorg/disgo/discord"
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func backupMessage(d *DBot, m discord.Message) error {
	if m.Author.Bot {
		return nil
	}

	var err error
	_, attachments, err := BackupAttachment(d, m)
	if err != nil {
		return err
	}

	_, err = d.Backup.UpsertUser(d.Ctx, backup.UpsertUserParams{
		Username:  m.Author.Username,
		DiscordID: must(strconv.ParseInt(m.Author.ID.String(), 10, 64)),
	})
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

	err = d.Backup.InsertBackup(d.Ctx, backup.InsertBackupParams{
		MsgID:       must(strconv.ParseInt(m.ID.String(), 10, 64)),
		AuthorID:    must(strconv.ParseInt(m.Author.ID.String(), 10, 64)),
		ChannelID:   must(strconv.ParseInt(m.ChannelID.String(), 10, 64)),
		Content:     m.Content,
		Attachments: attachments,
		CreatedAt:   time.Now(),
	})
	if err != nil {
		return fmt.Errorf("insert backup: %w", err)
	}

	return nil
}

func updateBackupMessage(d *DBot, m discord.Message) error {
	err := d.Backup.UpdateBackupMsg(d.Ctx, backup.UpdateBackupMsgParams{
		MsgID:   must(strconv.ParseInt(m.ID.String(), 10, 64)),
		Content: m.Content,
	})
	if err != nil {
		return fmt.Errorf("updateBackupMessage: %w", err)
	}

	return nil
}

func BackupAttachment(d *DBot, m discord.Message) (BackupFile, string, error) {
	if len(m.Attachments) == 0 {
		return BackupFile{}, "", nil
	}

	var gid string
	if m.GuildID != nil {
		gid = m.GuildID.String()
	}

	var info BackupFile
	att := make([]string, len(m.Attachments))
	for i, a := range m.Attachments {
		slog.Info("downloading attachment", slog.String("url", a.URL))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return BackupFile{}, "", fmt.Errorf("failed to create request '%s': %w", a.URL, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return BackupFile{}, "", fmt.Errorf("failed to get '%s': %w", a.URL, err)
		}
		defer resp.Body.Close()

		info, err = d.backupArtefact(d.Ctx, BackupFileParams{
			Name:        a.Filename,
			ContentType: resp.Header.Get("content-type"),
			Dirs:        "attachments",
			PrependTime: true,
			OriginUrl:   a.URL,
			GID:         gid,
			CHID:        m.ChannelID.String(),
			MSGID:       m.ID.String(),
			File:        resp.Body,
		})
		if err != nil {
			return BackupFile{}, "", fmt.Errorf("backup artefact: %w", err)
		}

		att[i] = info.Name
	}

	b, err := json.Marshal(att)
	if err != nil {
		return BackupFile{}, "", fmt.Errorf("encode attachments: %w", err)
	}

	return info, string(b), nil
}

const (
	JPG  = "image/jpg"
	JPEG = "image/jpeg"
	PNG  = "image/png"
	WEBP = "image/webp"
)

func (d *DBot) backupArtefact(ctx context.Context, params BackupFileParams) (BackupFile, error) {
	var buf bytes.Buffer
	params.File = io.TeeReader(params.File, &buf)
	info, err := d.backupFile(params)
	if err != nil {
		return info, fmt.Errorf("backup artefact '%s': %w", params.Name, err)
	}

	hash, err := generateHash(params.ContentType, &buf)
	if err != nil {
		slog.Error("img hash failed", slog.Any("error", err), slog.String("content", params.ContentType), slog.String("url", params.OriginUrl))
	}

	slog.Debug("[dupa]", slog.Uint64("hash", hash), slog.String("content", params.ContentType))
	if hash > 0 {
		art, unique := d.checkHashAgainstDB(params.GID, hash)
		if !unique {
			info.PossibleDupe = &art
		}
	}

	err = d.Backup.InsertArtefact(ctx, backup.InsertArtefactParams{
		Path:      info.Name,
		MediaType: params.ContentType,
		OriginUrl: params.OriginUrl,
		Hash:      int64(hash),
		CreatedAt: time.Now(),
		Gid:       params.GID,
		Chid:      params.CHID,
		Msgid:     params.MSGID,
	})
	if err != nil {
		return info, fmt.Errorf("insert artefact: %w", err)
	}

	return info, nil
}

func (d *DBot) checkHashAgainstDB(gid string, hash uint64) (art backup.Artefact, unique bool) {
	hash1 := goimagehash.NewExtImageHash([]uint64{hash}, goimagehash.DHash, 64)
	offset := int64(0)
	do := true
	for do {
		data, err := d.Backup.GetArtefacts(d.Ctx, backup.GetArtefactsParams{
			Gid:    gid,
			Offset: offset,
		})
		if err != nil {
			slog.Error("failed to get Artiefacts", slog.Any("error", err))
			return backup.Artefact{}, true
		}
		for _, d := range data {
			hash2 := goimagehash.NewExtImageHash([]uint64{uint64(d.Hash)}, goimagehash.DHash, 64)
			dist, err := hash1.Distance(hash2)
			if err != nil {
				slog.Error("failed to calc distance", slog.Any("error", err))
			}
			if dist <= 3 {
				slog.Debug("[dupa]", slog.Any("dist", dist))
				return d, false
			}
		}

		do = len(data) == 100
		offset += 100
	}

	return backup.Artefact{}, true
}

func generateHash(content string, f io.Reader) (uint64, error) {
	var img image.Image
	var err error
	fmt.Println(content)
	switch content {
	case PNG:
		img, err = png.Decode(f)
	case JPG, JPEG:
		img, err = jpeg.Decode(f)
	case WEBP:
		img, err = webp.Decode(f)
	default:
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	hash, err := goimagehash.DifferenceHash(img)
	if err != nil {
		return 0, err
	}

	return hash.GetHash(), nil
}

type BackupFile struct {
	Name         string
	Size         int64
	PossibleDupe *backup.Artefact
}

type BackupFileParams struct {
	Name        string
	Dirs        string
	GID         string
	CHID        string
	MSGID       string
	ContentType string
	OriginUrl   string
	PrependTime bool
	File        io.Reader
}

func (d *DBot) backupFile(params BackupFileParams) (BackupFile, error) {
	var backupFile BackupFile
	dir := filepath.Join(config.BACKUP_DIR, params.GID, params.Dirs)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return backupFile, fmt.Errorf("failed to create dir: %w", err)
	}

	name := filepath.Base(params.Name)
	if params.PrependTime {
		name = fmt.Sprintf("%d-%s", time.Now().Unix(), params.Name)
	}

	pf := filepath.Join(dir, name)
	f, err := os.Create(pf)
	if err != nil {
		return backupFile, fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, params.File)
	if err != nil {
		return backupFile, fmt.Errorf("failed to copy file: %w", err)
	}

	slog.Debug("backupFile", slog.String("name", pf), slog.Int64("size", n))
	backupFile.Name = strings.TrimLeft(f.Name(), config.BACKUP_DIR)
	backupFile.Size = n

	return backupFile, nil
}
