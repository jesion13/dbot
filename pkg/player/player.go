package player

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dbot/pkg/config"
	"dbot/pkg/ytdlp"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/fr-str/log"
	"github.com/pion/opus/pkg/oggreader"
)

// framePacer paces opus frame sends to real-time (20ms/frame), matching
// disgo's own voice.AudioSender timing. We can't use AudioSender/
// SetOpusFrameProvider directly: it assumes exclusive ownership of the
// Conn, but music and sounds are two independent producers that both write
// to the same Conn (a sound can interrupt a song mid-stream) - handing
// ownership to a second provider silently abandons the first one, which
// is why using it caused the song to never resume after a sound played.
type framePacer struct {
	lastFrameSent int64
}

func (fp *framePacer) wait() {
	if fp.lastFrameSent == 0 {
		fp.lastFrameSent = time.Now().UnixMilli()
		return
	}

	sleepTime := time.Duration(int64(voice.OpusFrameSizeMs) - (time.Now().UnixMilli() - fp.lastFrameSent))
	if sleepTime > 0 {
		time.Sleep(sleepTime * time.Millisecond)
	}
	if time.Now().UnixMilli() < fp.lastFrameSent+int64(voice.OpusFrameSizeMs)*3 {
		fp.lastFrameSent += int64(voice.OpusFrameSizeMs)
	} else {
		fp.lastFrameSent = time.Now().UnixMilli()
	}
}

func (p *Player) playerErr(msg string, vars ...any) Err {
	return Err{
		Err: fmt.Errorf("player: "+msg+": %w", vars...),
		GID: p.VC.GuildID().String(),
	}
}

func (t *Player) PlayPause() {
	if !t.paused {
		t.Lock()
		t.paused = true
		t.Playing.Store(false)
		return
	}

	t.Playing.Store(true)
	t.paused = false
	t.Unlock()
}

type Player struct {
	ytdlp.YTDLP

	VCID    snowflake.ID
	list    list
	VC      voice.Conn
	ErrChan chan Err

	// play pause
	sync.Mutex
	paused  bool
	Playing atomic.Bool

	// soundboard
	queue chan *Audio
}

type Err struct {
	GID string
	Err error
}

func NewPlayer() *Player {
	p := &Player{
		list:    newList(),
		queue:   make(chan *Audio, 1000),
		ErrChan: make(chan Err),
	}
	go p.musicLoop()
	go p.soundLoop()

	return p
}

func (p *Player) Close() {
	for i := range p.list.list {
		os.Remove(p.list.list[i].Filepath)
	}
}

func (p *Player) Current() *Audio {
	return p.list.current()
}

func (p *Player) Next() *Audio {
	return p.list.peek()
}

func (p *Player) Add(link string) {
	p.list.add(link)
	if p.paused {
		p.PlayPause()
	}
}

func (p *Player) PlaySound(link string) {
	a := &Audio{Link: link}
	err := p.fetch(a)
	if err != nil {
		p.ErrChan <- p.playerErr("failed to download", err)
		return
	}
	p.queue <- a
}

func (p *Player) musicLoop() {
	for a := range p.list.nextAudio {
		log.Debug("list.nextAudio", log.JSON(a))
		if len(a.Filepath) == 0 && strings.Contains(a.Link, p.VC.GuildID().String()) {
			a.Filepath = filepath.Join(config.BACKUP_DIR, a.Link)
		}

		err := p.playV2(a)
		if err != nil {
			p.ErrChan <- p.playerErr("failed to play", err)
			continue
		}

		log.Trace("musicLoop", log.Bool("p.list.more()", p.list.more()))
		if !p.list.more() {
			p.PlayPause()
		}

		// Locking allows us to wait if p.list.more() returns false
		// after user add aditional entry p.PlayPause will be trigered unlocking the mutex
		p.Lock()
		p.list.next()
		p.Unlock()
	}
}

func (p *Player) soundLoop() {
	for a := range p.queue {
		var shouldUnpause bool
		if !p.paused {
			shouldUnpause = true
			p.PlayPause()
		}
		err := p.playSound(a)
		if err != nil {
			p.ErrChan <- p.playerErr("failed to play", err)
			continue
		}

		if shouldUnpause {
			p.PlayPause()
		}
	}
}

func (p *Player) fetch(audio *Audio) error {
	if strings.Contains(audio.Link, p.VC.GuildID().String()) {
		audio.Filepath = filepath.Join(config.BACKUP_DIR, audio.Link)
		return nil
	}

	meta, err := p.YTDLP.DownloadAudio(audio.Link)
	if err != nil {
		return err
	}

	audio.Filepath = meta.Filepath
	audio.Title = meta.Title
	audio.Link = meta.OriginalURL
	return nil
}

func (p *Player) playV2(audio *Audio) error {
	var err error
	var pipe io.ReadCloser
	log.Debug("playV2", log.JSON(audio))
	ytdlpCMD := exec.Command("yt-dlp", "--no-playlist", "-f", "bestaudio", "-o", "-", audio.Link)
	if audio.Filepath == "" {
		pipe, err = ytdlpCMD.StdoutPipe()
	} else {
		pipe, err = os.Open(audio.Filepath)
	}
	if err != nil {
		return err
	}

	ffmpegCMD := exec.Command(
		"ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", "-", // audio.Filepath,
		"-ar", "48000",
		"-ac", "2",
		"-af", dynaudnorm,
		"-c:a", "libopus",
		"-frame_duration", "20",
		"-vbr", "off",
		"-b:a", "64k",
		"-application", "audio",
		"-packet_loss", "0",
		"-f", "opus",
		"pipe:1",
	)
	ffmpegCMD.Stdin = pipe
	ffmpegCMD.Stderr = os.Stderr

	audioStream, err := ffmpegCMD.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create Stdout pipe: %w", err)
	}

	if audio.Filepath == "" {
		ytdlpCMD.Stderr = os.Stderr
		err = ytdlpCMD.Start()
		if err != nil {
			return fmt.Errorf("cmd.Start failed: %w", err)
		}
	}

	err = ffmpegCMD.Start()
	if err != nil {
		return fmt.Errorf("cmd.Start failed: %w", err)
	}

	reader, _, err := oggreader.NewWith(audioStream)
	if err != nil {
		return fmt.Errorf("failed to create oggreader from stdout: %w", err)
	}

	ctx := context.Background()
	p.VC.SetSpeaking(ctx, voice.SpeakingFlagMicrophone)
	defer p.VC.SetSpeaking(ctx, voice.SpeakingFlagNone)
	var pacer framePacer
	for {
		page, _, err := reader.ParseNextPage()
		if err != nil {
			if err != io.EOF {
				log.Error("failed to parse page", log.Err(err))
			}
			break
		}

		for _, frame := range page {
			p.Lock()
			pacer.wait()
			_, err := p.VC.UDP().Write(frame)
			p.Unlock()
			if err != nil {
				log.Error("failed to write opus frame", log.Err(err))
			}
		}
	}

	// Wait for FFmpeg to finish
	ffmpegCMD.Wait()
	if audio.Filepath == "" {
		ytdlpCMD.Wait()
	}
	return nil
}

// const dynaudnorm = `dynaudnorm=f=150:g=15:p=0.9`

const dynaudnorm = `dynaudnorm=f=150:g=15:p=0.95:m=50.0`

func (p *Player) playSound(audio *Audio) error {
	p.Playing.Store(true)
	defer p.Playing.Store(false)
	log.Debug("playSound", log.JSON(audio))
	cmd := exec.Command(
		"ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", audio.Filepath,
		"-ar", "48000",
		"-ac", "2",
		"-c:a", "libopus",
		"-frame_duration", "20",
		"-af", dynaudnorm,
		"-vbr", "off",
		"-b:a", "64k",
		"-application", "audio",
		"-packet_loss", "0",
		"-f", "opus",
		"pipe:1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create Stdout pipe: %w", err)
	}

	log.Info("playSound", log.String("cmd", cmd.String()))
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("cmd.Start failed: %w", err)
	}

	reader, _, err := oggreader.NewWith(stdout)
	if err != nil {
		return fmt.Errorf("failed to create oggreader from stdout: %w", err)
	}

	ctx := context.Background()
	p.VC.SetSpeaking(ctx, voice.SpeakingFlagMicrophone)
	defer p.VC.SetSpeaking(ctx, voice.SpeakingFlagNone)
	var pacer framePacer
	for {
		page, _, err := reader.ParseNextPage()
		if err != nil {
			if err != io.EOF {
				log.Error("failed to parse page", log.Err(err))
			}
			break
		}

		for _, frame := range page {
			pacer.wait()
			if _, err := p.VC.UDP().Write(frame); err != nil {
				log.Error("failed to write opus frame", log.Err(err))
			}
		}
	}

	// Wait for FFmpeg to finish
	return cmd.Wait()
}
