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
func (r *panelRegistry) publish(gid, channelID snowflake.ID, send func(generation uint64) (snowflake.ID, error)) (prev, next *panelEntry, err error) {
	// The registry mutex is released around the send: holding it across a REST call
	// would stall every other guild's panel behind one slow request. Callers already
	// hold the per-guild update lock, so publishes for one guild stay serialized.
	r.mu.Lock()
	var (
		generation uint64 = 1
		keepWorker context.CancelFunc
	)
	if st := r.byGID[gid]; st != nil {
		generation = st.entry.generation + 1
		keepWorker = st.cancel
	}
	r.mu.Unlock()

	msgID, err := send(generation)
	if err != nil {
		// the previous panel stays registered and usable
		return nil, nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byGID == nil {
		r.byGID = map[snowflake.ID]*panelState{}
	}
	if st := r.byGID[gid]; st != nil {
		old := st.entry
		prev = &old
	}
	entry := panelEntry{channelID: channelID, messageID: msgID, generation: generation}
	r.byGID[gid] = &panelState{entry: entry, cancel: keepWorker}
	next = &entry
	return prev, next, nil
}

// update rewrites the guild's panel where it already is, leaving the message id and
// the generation untouched.
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

// rememberPanel records which message is the guild's panel, so a restart or a deploy
// can still find and delete it. Best effort: losing the record costs an orphaned
// message, so it must never fail the publish that is under way.
func (d *DBot) rememberPanel(ctx context.Context, gid snowflake.ID, e panelEntry) {
	if d.rememberPanelRow == nil {
		return
	}
	if err := d.rememberPanelRow(ctx, gid, e); err != nil {
		slog.Error("player panel not remembered", slog.Any("error", err))
	}
}

// forgetPanel drops the record once the message is gone, so startup does not go
// looking for one that is already deleted.
func (d *DBot) forgetPanel(ctx context.Context, gid snowflake.ID) {
	if d.forgetPanelRow == nil {
		return
	}
	if err := d.forgetPanelRow(ctx, gid); err != nil {
		slog.Error("player panel not forgotten", slog.Any("error", err))
	}
}

// cleanupOrphanPanel deletes the panel a previous run left behind. Called once at
// startup: without it every restart or deploy accumulates a panel nobody owns.
func (d *DBot) cleanupOrphanPanel(ctx context.Context, gid snowflake.ID) {
	if d.loadPanelRow == nil {
		return
	}
	old, err := d.loadPanelRow(ctx, gid)
	if err != nil || old.MessageID == "" {
		return // nothing recorded, or already gone
	}

	channelID, err1 := snowflake.Parse(old.ChannelID)
	messageID, err2 := snowflake.Parse(old.MessageID)
	if err1 != nil || err2 != nil {
		return
	}

	if err := d.deletePanelMessage(channelID, messageID); err != nil {
		slog.Warn("could not delete the panel from a previous run",
			slog.String("channel", old.ChannelID),
			slog.String("message", old.MessageID),
			slog.Any("error", err))
		return
	}
	d.forgetPanel(ctx, gid)
	slog.Info("deleted the player panel left by a previous run")
}

// storedPanel is the persisted identity of a guild's panel.
type storedPanel struct {
	ChannelID string
	MessageID string
}

// cleanupOrphanPanelOnce deletes a panel left behind by a previous run, before
// anything publishes a new one.
func (d *DBot) cleanupOrphanPanelOnce(ctx context.Context) { d.cleanupOrphanPanel(ctx, d.GuildID) }

// panelIsNewest reports whether the guild's panel is still the last message in its
// channel. If it is, a refresh can rewrite it in place and every button on it stays
// valid; if anything was posted after it, the panel has to move back to the bottom,
// which means a new message. A lookup failure is treated as "still newest": editing
// in place is the cheap, safe fallback.
func (d *DBot) panelIsNewest(e panelEntry) bool {
	if d.newestPanel != nil {
		ok, err := d.newestPanel(e.channelID, e.messageID)
		return err != nil || ok
	}

	messages, err := d.Client.Rest.GetMessages(e.channelID, 0, 0, 0, 1)
	if err != nil || len(messages) == 0 {
		return true
	}
	return messages[0].ID == e.messageID
}

// refreshGuild republishes the panel only when it is no longer the newest message,
// and otherwise rewrites it where it is. Posting every few seconds instead would
// retire the panel while somebody was still reading it, which is what made button
// presses land on messages that were already superseded.
func (d *DBot) refreshGuild(ctx context.Context, gid snowflake.ID) error {
	cur, ok := d.panels.active(gid)
	if !ok {
		return nil
	}

	// Nothing queued means nothing to say. Rewriting the panel to repeat "STOPPED" every
	// ten seconds would burn an edit and a generation-visible message change for no
	// information, so the panel is left exactly as it was.
	if len(d.Player().QueueSnapshot().Tracks) == 0 {
		return nil
	}

	if d.panelIsNewest(cur) {
		return d.panels.update(gid, func(e panelEntry) error {
			return d.editPanelMessage(ctx, e.channelID, e.messageID,
				buildPlayerPanelV2(e.generation, d.Player().QueueSnapshot()))
		})
	}

	// re-read under the lock: a click may have replaced the panel while we waited
	cur, ok = d.panels.active(gid)
	if !ok {
		return nil
	}
	return d.swapPanel(ctx, gid, cur.channelID, func(generation uint64) (snowflake.ID, error) {
		return d.createPanelMessage(ctx, cur.channelID, buildPlayerPanelV2(generation, d.Player().QueueSnapshot()))
	})
}

// swapPanel publishes a replacement and only then retires the old one. The delete
// happens after the replacement is registered, so a failure anywhere leaves a
// working panel behind. Callers must already hold the guild's update lock.
func (d *DBot) swapPanel(ctx context.Context, gid, channelID snowflake.ID, send func(generation uint64) (snowflake.ID, error)) error {
	prev, next, err := d.panels.publish(gid, channelID, send)
	if err != nil {
		return err
	}

	// the record is written once the message exists, and only then: a send that failed
	// has left nothing behind worth remembering
	d.rememberPanel(ctx, gid, *next)

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
		return nil
	}
	// it is gone for good, so the record must move on or the next restart would go
	// looking for a message that no longer exists
	d.rememberPanel(ctx, gid, *next)
	return nil
}

// retirePanel replaces a panel we could not delete with a note saying so. A stale
// panel that still looks live is worse than useless: people click it and are told the
// panel is gone. Best effort - if this fails too the guard still refuses the click.
func (d *DBot) retirePanel(prev *panelEntry) {
	// A Components V2 message rejects a plain content update, so this sends another
	// V2 payload: the notice, and every control disabled.
	if err := d.editPanelMessage(d.botCtx(), prev.channelID, prev.messageID, retiredPanelMessage()); err != nil {
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

	// every /player publishes a replacement, even in the channel that already holds
	// one: the panel must be the newest message there
	err := d.swapPanel(ctx, *gid, channelID, func(generation uint64) (snowflake.ID, error) {
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
// bindPanelStore points the panel at the database so the active message survives a
// restart. Bound once at startup; nil until then, which the helpers treat as "do
// nothing" rather than an error.
func (d *DBot) bindPanelStore(ctx context.Context) {
	if d.Store == nil {
		return
	}
	d.loadPanelRow = func(_ context.Context, gid snowflake.ID) (storedPanel, error) {
		row, err := d.Store.GetPlayerPanel(ctx, gid.String())
		return storedPanel{ChannelID: row.ChannelID, MessageID: row.MessageID}, err
	}
	d.rememberPanelRow = func(_ context.Context, gid snowflake.ID, e panelEntry) error {
		_, err := d.Store.UpsertPlayerPanel(ctx, store.UpsertPlayerPanelParams{
			GuildID:    gid.String(),
			ChannelID:  e.channelID.String(),
			MessageID:  e.messageID.String(),
			Generation: int64(e.generation),
		})
		return err
	}
	d.forgetPanelRow = func(_ context.Context, gid snowflake.ID) error {
		return d.Store.DeletePlayerPanel(ctx, gid.String())
	}
}

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

	err = d.swapPanel(ctx, d.GuildID, channelID, func(generation uint64) (snowflake.ID, error) {
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

// refreshPanel republishes on every tick, whether or not anything changed, so the
// panel is always the newest message in the channel.
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
// interval. It always publishes; there is no "nothing changed" short circuit.
func (d *DBot) refreshOnce(ctx context.Context, gid snowflake.ID) error {
	unlock := d.panels.lockUpdate(gid)
	defer unlock()

	return d.refreshGuild(ctx, gid)
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
	// The event button joins a voice channel itself, so it is the one action that may
	// be pressed with no connection; everything else needs the bot already in one.
	if c.Action != playerPanelBtnEvent {
		if reason := d.voiceGuard(c.GuildID, c.UserID); reason != "" {
			return panelClickResult{Reject: reason}
		}
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

// queueErrText turns a player error into something a user can act on.
func queueErrText(err error, fallback string) string {
	switch {
	case errors.Is(err, player.ErrNoPreviousTrack):
		return "there is no earlier track in the queue"
	case errors.Is(err, player.ErrNoNextTrack):
		return "the queue is empty"
	case errors.Is(err, player.ErrInvalidPosition):
		return err.Error()
	case errors.Is(err, player.ErrClosed):
		return "the player is closed"
	case errors.Is(err, player.ErrSoundQueueFull):
		return "the sound queue is full"
	default:
		return fallback
	}
}

// panelAction maps one button onto the existing player or sound path. It builds no
// queue of its own and keeps no state.
func (d *DBot) panelAction(action string, gid, userID snowflake.ID) error {
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
		return d.playEvent(gid, userID)
	default:
		return fmt.Errorf("unknown player panel action %q", action)
	}
	return nil
}

// The listener has no context to give, so the handler takes the bot's.
func (d *DBot) onPanelComponentEvent(e *events.ComponentInteractionCreate) {
	d.onPanelComponent(d.botCtx(), e)
}

func (d *DBot) onPanelComponent(ctx context.Context, e *events.ComponentInteractionCreate) {
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

	// The refusal is the initial response here, so nothing has been acknowledged yet.
	res := d.resolvePanelClick(c)
	if res.Reject != "" {
		d.replyEphemeral(e, res.Reject)
		return
	}

	// Acknowledged before the action runs, so a slow sound lookup cannot let the
	// interaction expire. disgo's Acknowledge sends response type 2, which discord
	// no longer accepts, so the silent deferred update is the ack that actually works.
	// This is the only initial response: anything after it is a follow-up.
	if err := e.DeferUpdateMessage(); err != nil {
		slog.Error("player panel ack failed", slog.Any("error", err))
		return
	}

	if err := d.panelAction(c.Action, *gid, c.UserID); err != nil {
		slog.Error("player panel action failed", slog.String("action", c.Action), slog.Any("error", err))
		d.replyFollowup(e, err.Error())
		return
	}

	err := d.refreshGuild(ctx, *gid)
	if err != nil {
		slog.Error("player panel publish failed", slog.Any("error", err))
		d.replyFollowup(e, "could not refresh the player panel: "+err.Error())
	}
}

// replyEphemeral is the initial response, for a press that is refused before anything
// has been acknowledged.
func (d *DBot) replyEphemeral(e *events.ComponentInteractionCreate, content string) {
	if err := e.CreateMessage(discord.MessageCreate{
		Content: truncate(content),
		Flags:   discord.MessageFlagEphemeral,
	}); err != nil {
		slog.Error("player panel reply failed", slog.Any("error", err))
	}
}

// replyFollowup talks to the user after the press was acknowledged, where a second
// initial response is not allowed.
func (d *DBot) replyFollowup(e *events.ComponentInteractionCreate, content string) {
	msg := discord.MessageCreate{Content: truncate(content), Flags: discord.MessageFlagEphemeral}
	if d.followupPanel != nil {
		if err := d.followupPanel(e.Token(), msg); err != nil {
			slog.Error("player panel follow-up failed", slog.Any("error", err))
		}
		return
	}
	if _, err := d.Client.Rest.CreateFollowupMessage(d.Client.ApplicationID, e.Token(), msg); err != nil {
		slog.Error("player panel follow-up failed", slog.Any("error", err))
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

// playEvent is `event 1` from the panel: the same join-then-enqueue path the text
// command takes. A panel left behind in a channel the bot has already left voice
// still works, because the press puts the bot back into the clicker's channel.
// Runs after the interaction is acknowledged, so a slow voice open cannot expire it.
func (d *DBot) playEvent(gid, userID snowflake.ID) error {
	if err := d.connectVoice(gid, userID); err != nil {
		return errors.New("join a voice channel first")
	}
	return d.queueRandomSound(gid.String())
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
