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

const (
	panelBtnPrefix = "dbot:panel:"
	panelBtnPrev   = "prev"
	panelBtnPause  = "pause"
	panelBtnNext   = "next"
	panelBtnEvent  = "event"

	// the one place the refresh cadence is set
	panelRefresh = 10 * time.Second
)

const (
	// how much of the lineup either side of the current track the panel shows
	panelBefore = 3
	panelAfter  = 10
	// points at the playing row. The arrow is the only marker, and every other row is
	// padded to the same width, so the numbers stay flush left and aligned.
	panelNowMarker = "▶ "
)

// panelCustomID carries the generation the panel was published with. A click from a
// panel that has since been replaced carries an older one, so it is refused even if
// the old message was never deleted.
func panelCustomID(generation uint64, action string) string {
	return fmt.Sprintf("%s%d:%s", panelBtnPrefix, generation, action)
}

func parsePanelCustomID(customID string) (generation uint64, action string, ok bool) {
	rest, found := strings.CutPrefix(customID, panelBtnPrefix)
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

// --- layout ------------------------------------------------------------------

// panelRow is one visible queue line. Position is absolute and 1-based, so a number
// the user read minutes ago still points at the same track.
type panelRow struct {
	Position int
	Title    string
	Current  bool
}

// panelWindow is the slice of the lineup the panel shows, clipped to what exists.
// It is derived from CurrentIndex alone: nothing here tracks playback history.
func panelWindow(s player.QueueSnapshot) []panelRow {
	n := len(s.Tracks)
	if n == 0 {
		return nil
	}

	first, last := 1, min(n, panelAfter)
	if s.CurrentIndex > 0 {
		first = max(1, s.CurrentIndex-panelBefore)
		last = min(n, s.CurrentIndex+panelAfter)
	}

	rows := make([]panelRow, 0, last-first+1)
	for i := first; i <= last; i++ {
		rows = append(rows, panelRow{
			Position: i,
			Title:    s.Tracks[i-1].Title,
			Current:  i == s.CurrentIndex,
		})
	}
	return rows
}

type panelRender struct {
	components []discord.LayoutComponent
}

func renderPanel(generation uint64, s player.QueueSnapshot) panelRender {
	head := panelHead(s)
	body := panelBody(panelWindow(s))
	foot := panelFoot(s)
	return panelRender{components: panelComponents(generation, head, body, foot, s.Paused)}
}

func panelHead(s player.QueueSnapshot) string {
	status := "Stopped"
	if s.Current() != nil {
		status = "Playing"
		if s.Paused {
			status = "Paused"
		}
	}
	return "## DBot Player\n" + status
}

func panelBody(rows []panelRow) string {
	if len(rows) == 0 {
		return "Nothing queued yet."
	}

	var b strings.Builder
	for _, r := range rows {
		marker := strings.Repeat(" ", len([]rune(panelNowMarker)))
		if r.Current {
			marker = panelNowMarker
		}
		fmt.Fprintf(&b, "%s%d. %s\n", marker, r.Position, displayTitle(r.Title))
	}
	return strings.TrimRight(b.String(), "\n")
}

func panelFoot(s player.QueueSnapshot) string {
	switch s.SoundWait {
	case 0:
		return ""
	case 1:
		return "1 sound sequence waiting"
	default:
		return fmt.Sprintf("%d sound sequences waiting", s.SoundWait)
	}
}

func panelComponents(generation uint64, head, body, foot string, paused bool) []discord.LayoutComponent {
	id := func(action string) string { return panelCustomID(generation, action) }

	sub := []discord.ContainerSubComponent{
		discord.NewTextDisplay(head),
		discord.NewTextDisplay(body),
	}
	if foot != "" {
		sub = append(sub, discord.NewTextDisplay(foot))
	}
	sub = append(sub, discord.ActionRowComponent{}.WithComponents(
		discord.NewSecondaryButton("Previous", id(panelBtnPrev)),
		discord.NewSecondaryButton(pauseLabel(paused), id(panelBtnPause)),
		discord.NewSecondaryButton("Next", id(panelBtnNext)),
		discord.NewSecondaryButton("Event", id(panelBtnEvent)),
	))

	return []discord.LayoutComponent{discord.NewContainer(sub...)}
}

func pauseLabel(paused bool) string {
	if paused {
		return "Play"
	}
	return "Pause"
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

// swapPanel publishes a replacement and only then retires the old one. The send
// happens under the registry lock; the delete happens after the replacement is
// registered, so a failure anywhere leaves a working panel behind.
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
		slog.Error("player panel delete failed", slog.Any("error", err))
	}
	return nil
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

	err := d.swapPanel(*gid, channelID, func(generation uint64) (snowflake.ID, error) {
		r := renderPanel(generation, d.Player().QueueSnapshot())
		return d.respondPanel(e.ID(), e.Token(), discord.NewMessageCreateV2(r.components...))
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

	err = d.swapPanel(d.GuildID, channelID, func(generation uint64) (snowflake.ID, error) {
		r := renderPanel(generation, d.Player().QueueSnapshot())
		return d.createPanelMessage(ctx, channelID, r.components...)
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

// refreshPanel republishes on every tick, whether or not anything changed: the panel
// is meant to be the latest message in the channel, not merely an accurate one.
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
	cur, ok := d.panels.active(gid)
	if !ok {
		return nil
	}
	return d.swapPanel(gid, cur.channelID, func(generation uint64) (snowflake.ID, error) {
		r := renderPanel(generation, d.Player().QueueSnapshot())
		return d.createPanelMessage(ctx, cur.channelID, r.components...)
	})
}

// --- interactions ------------------------------------------------------------

// panelClick is one button press reduced to the fields the guards need.
type panelClick struct {
	GuildID    snowflake.ID
	UserID     snowflake.ID
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
	if !ok || cur.messageID != c.MessageID || cur.generation != c.Generation {
		return panelClickResult{Reject: "this panel is no longer active, run /player again"}
	}
	return panelClickResult{Panel: cur}
}

// panelAction maps one button onto the existing player or sound path. It builds no
// queue of its own and keeps no state.
func (d *DBot) panelAction(action string, gid snowflake.ID) error {
	switch action {
	case panelBtnPrev:
		if err := d.Player().Previous(); err != nil {
			return errors.New(queueErrText(err, "nothing to go back to"))
		}
	case panelBtnPause:
		if err := d.Player().TogglePause(); err != nil {
			return errors.New(queueErrText(err, "could not toggle pause"))
		}
	case panelBtnNext:
		if err := d.Player().SkipNext(); err != nil {
			return errors.New(queueErrText(err, "nothing to skip to"))
		}
	case panelBtnEvent:
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
		MessageID:  e.Message.ID,
		Generation: generation,
		Action:     action,
	}

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

	// the new panel is the reply: one response, inside discord's deadline, and the
	// queue is up to date without waiting for the tick
	err := d.swapPanel(*gid, res.Panel.channelID, func(gen uint64) (snowflake.ID, error) {
		r := renderPanel(gen, d.Player().QueueSnapshot())
		return d.respondPanel(e.ID(), e.Token(), discord.NewMessageCreateV2(r.components...))
	})
	if err != nil {
		slog.Error("player panel publish failed", slog.Any("error", err))
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

func (d *DBot) createPanelMessage(ctx context.Context, channelID snowflake.ID, comps ...discord.LayoutComponent) (snowflake.ID, error) {
	if d.createPanel != nil {
		return d.createPanel(ctx, channelID, comps...)
	}
	m, err := d.Client.Rest.CreateMessage(channelID, discord.NewMessageCreateV2(comps...))
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
