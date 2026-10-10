package dbot

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"dbot/pkg/ytdlp"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func truncateTestContent() string { return strings.Repeat("ą", 3000) }

func TestDeliverDeferred(t *testing.T) {
	t.Run("edit succeeds", func(t *testing.T) {
		var followups int
		err := deliverDeferred("done",
			func(c string) error {
				assert.Equal(t, "done", c)
				return nil
			},
			func(string) error { followups++; return nil })

		assert.ErrorIs(t, err, ErrInteractionAnswered)
		assert.Equal(t, 0, followups, "no follow-up when the edit worked")
	})

	t.Run("edit fails, follow-up succeeds", func(t *testing.T) {
		var got string
		err := deliverDeferred("final message",
			func(string) error { return errors.New("edit rejected") },
			func(c string) error { got = c; return nil })

		assert.ErrorIs(t, err, ErrInteractionAnswered)
		assert.Equal(t, "final message", got)
	})

	t.Run("both fail", func(t *testing.T) {
		var followups int
		err := deliverDeferred("final message",
			func(string) error { return errors.New("edit rejected") },
			func(string) error { followups++; return errors.New("token expired") })

		// still consumed: the caller must not send another initial response
		assert.ErrorIs(t, err, ErrInteractionAnswered)
		assert.Equal(t, 1, followups)
	})

	t.Run("long content is truncated before delivery", func(t *testing.T) {
		var got string
		err := deliverDeferred(truncateTestContent(),
			func(c string) error { got = c; return nil },
			func(string) error { return nil })

		require.ErrorIs(t, err, ErrInteractionAnswered)
		assert.LessOrEqual(t, len(got), maxMessageLen)
	})
}

// --- interaction deferral ---------------------------------------------------

// newCommandEvent builds a real application command interaction with a Respond hook
// that records the response types the handler sends.
func newCommandEvent(t *testing.T, command string, options ...string) (*events.ApplicationCommandInteractionCreate, *[]discord.InteractionResponseType) {
	t.Helper()
	opts := ""
	for _, o := range options {
		opts += "," + o
	}
	raw := `{
		"id":"1","application_id":"2","type":2,"token":"tok","version":1,
		"guild_id":"438758201916129281","channel_id":"3",
		"member":{"user":{"id":"4","username":"u"}},
		"data":{"id":"5","type":1,"name":"` + command + `"` + opts + `}}`

	var aci discord.ApplicationCommandInteraction
	require.NoError(t, json.Unmarshal([]byte(raw), &aci))

	var types []discord.InteractionResponseType
	e := &events.ApplicationCommandInteractionCreate{ApplicationCommandInteraction: aci}
	e.Respond = func(t discord.InteractionResponseType, _ discord.InteractionResponseData, _ ...rest.RequestOpt) error {
		types = append(types, t)
		return nil
	}
	return e, &types
}

// runHandler runs a handler that will fail once it reaches the voice connection
// (there is no discord client in tests). Only the recorded responses matter.
func runHandler(t *testing.T, fn func() error) {
	t.Helper()
	defer func() { _ = recover() }()
	_ = fn()
}

func TestHandlePlayDefersPlaylistWork(t *testing.T) {
	tb := newTestBot(t)
	ctx := tb.Ctx
	playlistURL := "https://www.youtube.com/playlist?list=PL1"

	t.Run("known playlist defers before playback work", func(t *testing.T) {
		_, err := tb.saveSnapshot(ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
		require.NoError(t, err)

		e, types := newCommandEvent(t, "play", `"options":[{"type":3,"name":"link","value":"`+playlistURL+`"}]`)
		runHandler(t, func() error { return tb.handlePlay(ctx, e) })

		require.NotEmpty(t, *types)
		assert.Equal(t, discord.InteractionResponseTypeDeferredCreateMessage, (*types)[0],
			"a playlist must be acknowledged before the voice connect and queue walk")
	})

	t.Run("new playlist defers before metadata extraction", func(t *testing.T) {
		fetched := false
		tb.PlaylistInfoFunc = func(string) (ytdlp.PlaylistMeta, error) {
			fetched = true
			return testPlaylistMeta(t, "PLNEW", "new", "a"), nil
		}
		e, types := newCommandEvent(t, "play", `"options":[{"type":3,"name":"link","value":"https://www.youtube.com/playlist?list=PLNEW"}]`)
		runHandler(t, func() error { return tb.handlePlay(ctx, e) })

		require.NotEmpty(t, *types)
		assert.Equal(t, discord.InteractionResponseTypeDeferredCreateMessage, (*types)[0])
		assert.False(t, fetched, "the handler must stop at the voice connect, before any yt-dlp call")
	})

	t.Run("single video is not deferred", func(t *testing.T) {
		e, types := newCommandEvent(t, "play", `"options":[{"type":3,"name":"link","value":"https://www.youtube.com/watch?v=abc"}]`)
		runHandler(t, func() error { return tb.handlePlay(ctx, e) })

		// it may still answer with an error message, but never with a deferral
		for _, got := range *types {
			assert.NotEqual(t, discord.InteractionResponseTypeDeferredCreateMessage, got)
		}
	})
}

func TestHandlePlaylistAlwaysDefers(t *testing.T) {
	for _, update := range []bool{false, true} {
		name := "update=false"
		if update {
			name = "update=true"
		}
		t.Run(name, func(t *testing.T) {
			tb := newTestBot(t)
			pl, err := tb.saveSnapshot(tb.Ctx, testGID, testPlaylistMeta(t, "PL1", "dupa", "a"), false)
			require.NoError(t, err)

			updateOpt := ""
			if update {
				updateOpt = `,{"type":5,"name":"update","value":true}`
			}
			e, types := newCommandEvent(t, "playlist",
				`"options":[{"type":3,"name":"name","value":"`+itoa(pl.ID)+`"}`+updateOpt+`]`)

			runHandler(t, func() error { return tb.handlePlaylist(tb.Ctx, e) })

			require.NotEmpty(t, *types)
			assert.Equal(t, discord.InteractionResponseTypeDeferredCreateMessage, (*types)[0],
				"/playlist always does cache lookups, a voice connect and a queue walk")
			// exactly one interaction response: no second initial response after the
			// deferred edit is attempted
			for _, got := range (*types)[1:] {
				assert.NotEqual(t, discord.InteractionResponseTypeCreateMessage, got)
			}
		})
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }
