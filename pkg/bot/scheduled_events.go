package dbot

import (
	"log/slog"

	"dbot/pkg/player"

	"github.com/disgoorg/snowflake/v2"
	"github.com/go-co-op/gocron/v2"
)

func (d *DBot) StartScheduler() {
	s, err := gocron.NewScheduler()
	if err != nil {
		slog.Error(err.Error())
		return
	}

	// papaj
	j, err := s.NewJob(
		gocron.CronJob("37 21 * * *", false),
		gocron.NewTask(d.papaj),
	)
	if err != nil {
		slog.Error(err.Error())
	}
	slog.Debug(j.Name())

	j, err = s.NewJob(
		gocron.CronJob("0 16,18,20,22,0 * * 3", false),
		gocron.NewTask(d.środowaNoc),
	)
	if err != nil {
		slog.Error(err.Error())
	}

	slog.Debug(j.Name())
	s.Start()
}

func (d *DBot) papaj() {
	d.playScheduledSound("/492318912881491981/sounds/papaj.mp4")
}

func (d *DBot) środowaNoc() {
	d.playScheduledSound("/492318912881491981/sounds/srodowanoc.mp4")
}

// playScheduledSound connects if needed, then queues one sound.
func (d *DBot) playScheduledSound(rel string) {
	conn, _ := d.Player().Conn()
	if conn == nil {
		if err := d.findVoiceChannel(); err != nil {
			slog.Error(err.Error())
			return
		}
		if conn, _ = d.Player().Conn(); conn == nil {
			return
		}
	}
	d.playSoundFile(rel)
}

// playSoundFile queues a sound already stored in the shared backup dir.
func (d *DBot) playSoundFile(rel string) {
	err := d.Player().EnqueueSound(player.SoundTrack{Filepath: artefactFile(rel)})
	if err != nil {
		slog.Error("failed queueing sound", slog.String("file", rel), slog.Any("error", err))
	}
}

// only used for scheduled events
func (d *DBot) findVoiceChannel() error {
	var channelID *snowflake.ID
	for vs := range d.Client.Caches.VoiceStates(d.GuildID) {
		channelID = vs.ChannelID
		break
	}
	if channelID == nil {
		return nil
	}

	vc := d.Client.VoiceManager.CreateConn(d.GuildID)
	err := vc.Open(d.Ctx, *channelID, false, false)
	if err != nil {
		return err
	}
	return d.AttachVoiceConnection(vc, *channelID)
}
