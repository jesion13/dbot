package dbot

import (
	"github.com/disgoorg/snowflake/v2"
	"github.com/go-co-op/gocron/v2"
	"log/slog"
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
	if d.MusicPlayer.VC == nil {
		err := d.findVoiceChannel()
		if err != nil {
			slog.Error(err.Error())
			return
		}
	}

	if d.MusicPlayer.VC != nil {
		d.MusicPlayer.PlaySound("/492318912881491981/sounds/papaj.mp4")
	}
}

func (d *DBot) środowaNoc() {
	if d.MusicPlayer.VC == nil {
		err := d.findVoiceChannel()
		if err != nil {
			slog.Error(err.Error())
			return
		}
	}
	if d.MusicPlayer.VC != nil {
		d.MusicPlayer.PlaySound("/492318912881491981/sounds/srodowanoc.mp4")
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
	d.MusicPlayer.VC = vc
	d.MusicPlayer.VCID = *channelID
	return nil
}
