package dbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbot/pkg/player"
	"dbot/pkg/store"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

// the one place the refresh cadence is set
const panelRefresh = 10 * time.Second

func parsePanelCustomID(customID string) (generation uint64, action string, ok bool) {
	rest, found := strings.CutPrefix(customID, playerPanelBtnPrefix)
	if !found {
		return 0, "", false
	}
	gen, action, found := strings.Cut(rest, ":")
	if !found || action == "" {
		return 0, "", false
	}
	n, err := strconv.ParseUint(gen, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return n, action, true
}

// voiceGuard returns a user-facing reason this interaction is not allowed, or "" when
// it is. Shared by the text commands and the panel so both enforce the same rule.
func (d *DBot) voiceGuard(gid, userID snowflake.ID) string {
	conn, botChanID := d.Player().Conn()
	if conn == nil {
		return "i am not connected to a voice channel"
	}
	vs, err := d.getUserVC(gid, userID)
	if err != nil || vs.ChannelID == nil {
		return "join a voice channel first"
	}
	if *vs.ChannelID != botChanID {
		return "join my voice channel first"
	}
	return ""
}

// --- registry ----------------------------------------------------------------

type panelEntry struct {
	channelID snowflake.ID
	messageID snowflake.ID
	// generation increases on every replacement, so a click can prove which panel it
	// came from
	generation uint64
}

type panelState struct {
	entry panelEntry
	// cancel stops the refresh worker; nil when none is running for this guild
	cancel context.CancelFunc
}

type panelRegistry struct {
	mu    sync.Mutex
	byGID map[snowflake.ID]*panelState
	// one update lock per guild, held across a whole panel update rather than just
	// the send, so the timer and a button press cannot interleave
	locks map[snowflake.ID]*sync.Mutex
}

// lockUpdate serializes a whole panel update for one guild and returns its unlock.
// The registry mutex alone is not enough: it only covers the send, so a timer tick
// could still retire the panel a button press is acting on, and the panel that press
// just sent would be deleted moments later.
func (r *panelRegistry) lockUpdate(gid snowflake.ID) func() {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = map[snowflake.ID]*sync.Mutex{}
	}
	l, ok := r.locks[gid]
	if !ok {
		l = &sync.Mutex{}
		r.locks[gid] = l
	}
	r.mu.Unlock()

	l.Lock()
	return l.Unlock
}

// publish sends the guild's next panel and registers it, returning the panel it
// displaced. send runs with the registry locked, which is what stops a timer tick and
// a button press from both publishing. A failed send leaves the current panel
// registered and usable.
func (r *panelRegistry) publish(gid, channelID snowflake.ID, send func(generation uint64) (snowflake.ID, error)) (prev *panelEntry, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.byGID == nil {
		r.byGID = map[snowflake.ID]*panelState{}
	}
	st := r.byGID[gid]

	var generation uint64 = 1
	if st != nil {
		generation = st.entry.generation + 1
	}

	msgID, err := send(generation)
	if err != nil {
		return nil, err
	}

	if st != nil {
		old := st.entry
		prev = &old
	}
	next := &panelState{entry: panelEntry{channelID: channelID, messageID: msgID, generation: generation}}
	if st != nil {
		// the worker keeps running across a swap; only /player restarts it
		next.cancel = st.cancel
	}
	r.byGID[gid] = next
	return prev, nil
}

// update rewrites the guild's panel where it already is. The message id and the
// generation both stay the same, so a button the user is reaching for keeps working
// across the edit - which is the whole point. Posting a fresh message every few
// seconds instead would retire panels while they were being read.
func (r *panelRegistry) update(gid snowflake.ID, edit func(panelEntry) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.byGID[gid]
	if st == nil {
		return nil
	}
	return edit(st.entry)
}

func (r *panelRegistry) active(gid snowflake.ID) (panelEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.byGID[gid]
	if st == nil {
		return panelEntry{}, false
	}
	return st.entry, true
}

// stopWorker cancels the guild's refresh worker, if one is running.
func (r *panelRegistry) stopWorker(gid snowflake.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.byGID[gid]; st != nil && st.cancel != nil {
		st.cancel()
		st.cancel = nil
	}
}

// --- lifecycle ---------------------------------------------------------------

// swapPanel publishes a replacement and only then retires the old one. The delete
// happens after the replacement is registered, so a failure anywhere leaves a
// working panel behind. Callers must already hold the guild's update lock.
func (d *DBot) swapPanel(gid, channelID snowflake.ID, send func(generation uint64) (snowflake.ID, error)) error {
	prev, err := d.panels.publish(gid, channelID, send)
	if err != nil {
		return err
	}
	if prev == nil {
		return nil
	}

	// the old panel is already unregistered, so its buttons are refused even if this
	// delete fails and the message stays sitting in the channel
	if err := d.deletePanelMessage(prev.channelID, prev.messageID); err != nil {
		slog.Error("player panel delete failed",
			slog.String("channel", prev.channelID.String()),
			slog.String("message", prev.messageID.String()),
			slog.Any("error", err))
		d.retirePanel(prev)
	}
	return nil
}

// retirePanel replaces a panel we could not delete with a note saying so. A stale
// panel that still looks live is worse than useless: people click it and are told the
// panel is gone. Best effort - if this fails too the guard still refuses the click.
func (d *DBot) retirePanel(prev *panelEntry) {
	content := "_(this player panel is no longer active)_"
	update := discord.MessageUpdate{Content: &content}
	if d.editPanel != nil {
		if err := d.editPanel(d.botCtx(), prev.channelID, prev.messageID, update); err != nil {
			slog.Error("player panel retire failed", slog.Any("error", err))
		}
		return
	}
	if _, err := d.Client.Rest.UpdateMessage(prev.channelID, prev.messageID, update); err != nil {
		slog.Error("player panel retire failed", slog.Any("error", err))
	}
}

// handlePlayer publishes the guild's panel in this channel and retires whatever was
// active, wherever it was. The panel is the command's reply, so the interaction is
// answered inside discord's deadline.
func (d *DBot) handlePlayer(ctx context.Context, e *events.ApplicationCommandInteractionCreate) error {
	gid := e.GuildID()
	if gid == nil {
		return errors.New("the player panel only works in a server")
	}
	channelID := e.Channel().ID()

	unlock := d.panels.lockUpdate(*gid)
	defer unlock()

	// already in this channel: rewrite it rather than post a second one, so /player
	// stays idempotent. The interaction is still answered first - the panel is not
	// the reply here, so without this the command would time out.
	if cur, ok := d.panels.active(*gid); ok && cur.channelID == channelID {
		if _, err := d.respondPanel(e.ID(), e.Token(), discord.MessageCreate{
			Content: "player panel refreshed",
			Flags:   discord.MessageFlagEphemeral,
		}); err != nil {
			return fmt.Errorf("refresh player panel: %w", err)
		}
		if err := d.panels.update(*gid, func(e panelEntry) error {
			return d.editPanelMessage(ctx, e.channelID, e.messageID,
				buildPlayerPanelV2(e.generation, d.Player().QueueSnapshot()))
		}); err != nil {
			return fmt.Errorf("refresh player panel: %w", err)
		}
		d.startPanelWorker(*gid)
		return nil
	}

	// the panel is the command's reply, so the interaction is answered inside
	// discord's deadline and we get the message back to register
	err := d.swapPanel(*gid, channelID, func(generation uint64) (snowflake.ID, error) {
		return d.respondPanel(e.ID(), e.Token(), buildPlayerPanelV2(generation, d.Player().QueueSnapshot()))
	})
	if err != nil {
		return fmt.Errorf("create player panel: %w", err)
	}

	// an explicit /player replaces the worker: the new panel owns a fresh one
	d.startPanelWorker(*gid)
	return nil
}

// startPanelWorker (re)starts the guild's refresh worker. It hangs off the bot's
// context, never the caller's, because a command handler's context dies the moment
// the handler returns.
func (d *DBot) startPanelWorker(gid snowflake.ID) {
	d.panels.stopWorker(gid)

	workerCtx, cancel := context.WithCancel(d.botCtx())
	d.panels.setWorker(gid, cancel)
	go d.refreshPanel(workerCtx, gid)
}

// ensurePanel publishes a panel when music is playing and the guild has none, so
// playback never runs with no way to control it. Nothing is posted while nothing is
// playing, and an existing panel is left entirely alone - its own worker owns it.
func (d *DBot) ensurePanel(ctx context.Context) {
	t := time.NewTicker(panelRefresh)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := d.ensureOnce(ctx); err != nil {
			slog.Error("player panel: could not publish", slog.Any("error", err))
		}
	}
}

// ensureOnce is one tick of ensurePanel, separated so a test can drive it without
// waiting out the interval. A missing music channel is not an error: there is simply
// nowhere to post until one is mapped.
func (d *DBot) ensureOnce(ctx context.Context) error {
	if _, ok := d.panels.active(d.GuildID); ok {
		return nil
	}
	if d.Player().QueueSnapshot().Current() == nil {
		return nil
	}

	ch, err := d.Store.GetChannel(ctx, store.GetChannelParams{
		Gid:  d.GuildID.String(),
		Type: musicChannel,
	})
	if err != nil {
		slog.Debug("player panel: no music channel mapped yet", slog.Any("error", err))
		return nil
	}
	channelID, err := snowflake.Parse(ch.Chid)
	if err != nil {
		return fmt.Errorf("music channel id %q: %w", ch.Chid, err)
	}

	unlock := d.panels.lockUpdate(d.GuildID)
	defer unlock()

	err = d.swapPanel(d.GuildID, channelID, func(generation uint64) (snowflake.ID, error) {
		return d.createPanelMessage(ctx, channelID, buildPlayerPanelV2(generation, d.Player().QueueSnapshot()))
	})
	if err != nil {
		return err
	}

	slog.Info("player panel published", slog.String("channel", channelID.String()))
	d.startPanelWorker(d.GuildID)
	return nil
}

func (r *panelRegistry) setWorker(gid snowflake.ID, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byGID == nil {
		r.byGID = map[snowflake.ID]*panelState{}
	}
	st := r.byGID[gid]
	if st == nil {
		st = &panelState{}
		r.byGID[gid] = st
	}
	st.cancel = cancel
}

// refreshPanel rewrites the panel on every tick, whether or not anything changed, so
// what the channel shows is always current.
func (d *DBot) refreshPanel(ctx context.Context, gid snowflake.ID) {
	t := time.NewTicker(panelRefresh)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := d.refreshOnce(ctx, gid); err != nil {
			// no tight retry: the next tick tries again
			slog.Error("player panel refresh failed", slog.Any("error", err))
		}
	}
}

// refreshOnce is one tick, separated so a test can drive it without waiting out the
// interval. It always rewrites the panel; there is no "nothing changed" short
// circuit, so the message in the channel is always the current one.
func (d *DBot) refreshOnce(ctx context.Context, gid snowflake.ID) error {
	unlock := d.panels.lockUpdate(gid)
	defer unlock()

	return d.panels.update(gid, func(e panelEntry) error {
		return d.editPanelMessage(ctx, e.channelID, e.messageID,
			buildPlayerPanelV2(e.generation, d.Player().QueueSnapshot()))
	})
}

// --- interactions ------------------------------------------------------------

// panelClick is one button press reduced to the fields the guards need.
type panelClick struct {
	GuildID    snowflake.ID
	UserID     snowflake.ID
	ChannelID  snowflake.ID
	MessageID  snowflake.ID
	Generation uint64
	Action     string
}

type panelClickResult struct {
	Panel  panelEntry
	Reject string // non-empty: answer ephemerally with this and touch nothing
}

// resolvePanelClick only lets the registered panel drive playback. Both the message
// id and the generation must match, so a message that outlived its deletion is still
// refused.
func (d *DBot) resolvePanelClick(c panelClick) panelClickResult {
	if reason := d.voiceGuard(c.GuildID, c.UserID); reason != "" {
		return panelClickResult{Reject: reason}
	}
	cur, ok := d.panels.active(c.GuildID)
	switch {
	case !ok:
		slog.Warn("player panel: click with no panel registered",
			slog.String("message", c.MessageID.String()))
	case cur.channelID != c.ChannelID:
		slog.Warn("player panel: click from another channel",
			slog.String("clicked", c.ChannelID.String()),
			slog.String("active", cur.channelID.String()))
		return panelClickResult{Reject: "this panel belongs to another channel, run /player here"}
	case cur.messageID != c.MessageID:
		slog.Warn("player panel: click from a different message",
			slog.String("clicked", c.MessageID.String()),
			slog.String("active", cur.messageID.String()),
			slog.Duration("age", time.Since(snowflakeTimestamp(c.MessageID))-time.Since(snowflakeTimestamp(cur.messageID))),
			slog.Bool("clickedChannelMatches", c.ChannelID == cur.channelID))
	case cur.generation != c.Generation:
		slog.Warn("player panel: click from an older generation",
			slog.Uint64("clicked", c.Generation),
			slog.Uint64("active", cur.generation))
	default:
		return panelClickResult{Panel: cur}
	}
	return panelClickResult{Reject: "this panel is no longer active, run /player again"}
}

// panelAction maps one button onto the existing player or sound path. It builds no
// queue of its own and keeps no state.
func (d *DBot) panelAction(action string, gid snowflake.ID) error {
	switch action {
	case playerPanelBtnPrev:
		if err := d.Player().Previous(); err != nil {
			return errors.New(queueErrText(err, "nothing to go back to"))
		}
	case playerPanelBtnPause:
		if err := d.Player().TogglePause(); err != nil {
			return errors.New(queueErrText(err, "could not toggle pause"))
		}
	case playerPanelBtnNext:
		if err := d.Player().SkipNext(); err != nil {
			return errors.New(queueErrText(err, "nothing to skip to"))
		}
	case playerPanelBtnEvent:
		return d.queueRandomSound(gid.String())
	default:
		return fmt.Errorf("unknown player panel action %q", action)
	}
	return nil
}

func (d *DBot) onPanelComponent(e *events.ComponentInteractionCreate) {
	generation, action, ok := parsePanelCustomID(e.Data.CustomID())
	if !ok {
		return
	}
	gid := e.GuildID()
	if gid == nil {
		return
	}

	c := panelClick{
		GuildID:    *gid,
		UserID:     e.User().ID,
		ChannelID:  e.Channel().ID(),
		MessageID:  e.Message.ID,
		Generation: generation,
		Action:     action,
	}

	// the lock spans the guard, the action and the publish, so a timer tick cannot
	// retire the panel this press is acting on, or the panel it is about to send
	unlock := d.panels.lockUpdate(*gid)
	defer unlock()

	res := d.resolvePanelClick(c)
	if res.Reject != "" {
		d.replyEphemeral(e, res.Reject)
		return
	}
	if err := d.panelAction(c.Action, *gid); err != nil {
		slog.Error("player panel action failed", slog.String("action", c.Action), slog.Any("error", err))
		d.replyEphemeral(e, err.Error())
		return
	}

	// The interaction response is the panel rewrite: one call that answers inside
	// discord's deadline and leaves the refreshed panel on the message the user is
	// already looking at. Nothing must answer twice, and nothing may answer nothing.
	panel := buildPlayerPanelV2(res.Panel.generation, d.Player().QueueSnapshot())
	if err := e.UpdateMessage(discord.NewMessageUpdateV2(panel.Components...)); err != nil {
		slog.Error("player panel update failed", slog.Any("error", err))
		d.replyEphemeral(e, "could not refresh the player panel: "+err.Error())
	}
}

func (d *DBot) replyEphemeral(e *events.ComponentInteractionCreate, content string) {
	if err := e.CreateMessage(discord.MessageCreate{
		Content: truncate(content),
		Flags:   discord.MessageFlagEphemeral,
	}); err != nil {
		slog.Error("player panel reply failed", slog.Any("error", err))
	}
}

// Exactly one random saved sound, as one sequence. This is the same path `event 1`
// takes; the button simply has no count to ask for, so it never enqueues a run.
func (d *DBot) queueRandomSound(gid string) error {
	seq, err := randomSoundSequence(d.Store, gid)
	if err != nil {
		return err
	}
	return d.Player().EnqueueSoundSequence(seq)
}

func randomSoundSequence(db *store.Queries, gid string) (player.SoundSequence, error) {
	sounds, err := findSound(db, "event 1", gid)
	if err != nil {
		return player.SoundSequence{}, err
	}

	tracks := make([]player.SoundTrack, 0, len(sounds))
	for _, s := range sounds {
		tracks = append(tracks, soundTrack(s))
	}
	return player.SoundSequence{Tracks: tracks}, nil
}

// --- discord seams -----------------------------------------------------------
//
// Replaced in tests so nothing here needs a client or a network.

func (d *DBot) editPanelMessage(ctx context.Context, channelID, messageID snowflake.ID, msg discord.MessageCreate) error {
	update := discord.NewMessageUpdateV2(msg.Components...)
	if d.editPanel != nil {
		return d.editPanel(ctx, channelID, messageID, update)
	}
	_, err := d.Client.Rest.UpdateMessage(channelID, messageID, update)
	return err
}

// snowflakeTimestamp turns a discord id back into a time, so a log line can say how
// far out of date the panel someone clicked was.
func snowflakeTimestamp(id snowflake.ID) time.Time {
	const discordEpoch = 1420070400000 // 2015-01-01T00:00:00Z in unix millis
	return time.UnixMilli(int64(id>>22) + discordEpoch)
}

func (d *DBot) createPanelMessage(ctx context.Context, channelID snowflake.ID, msg discord.MessageCreate) (snowflake.ID, error) {
	if d.createPanel != nil {
		return d.createPanel(ctx, channelID, msg)
	}
	m, err := d.Client.Rest.CreateMessage(channelID, msg)
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

// respondPanel is a slash command or button reply; it returns the message it made.
func (d *DBot) respondPanel(interactionID snowflake.ID, token string, m discord.MessageCreate) (snowflake.ID, error) {
	if d.answerPanel != nil {
		return d.answerPanel(interactionID, token, m)
	}
	res, err := d.Client.Rest.CreateInteractionResponseWithCallback(interactionID, token, discord.InteractionResponse{
		Type: discord.InteractionResponseTypeCreateMessage,
		Data: m,
	})
	if err != nil {
		return 0, err
	}
	if res == nil || res.Resource == nil || res.Resource.Message == nil {
		return 0, errors.New("discord returned no message")
	}
	return res.Resource.Message.ID, nil
}

func (d *DBot) deletePanelMessage(channelID, messageID snowflake.ID) error {
	if d.deletePanel != nil {
		return d.deletePanel(channelID, messageID)
	}
	return d.Client.Rest.DeleteMessage(channelID, messageID)
}
