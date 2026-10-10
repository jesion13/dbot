package dbot

import (
	"fmt"
	"net/url"
	"strings"
)

// canonicalVideoURL is the single form used for new youtube artefact keys, playlist
// entries and backup lookups so the same video always resolves to the same row.
func canonicalVideoURL(videoID string) string {
	return fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)
}

func canonicalPlaylistURL(playlistID string) string {
	return fmt.Sprintf("https://www.youtube.com/playlist?list=%s", playlistID)
}

func isYouTubeHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	host = strings.TrimPrefix(host, "music.")
	switch host {
	case "youtube.com", "youtu.be":
		return true
	}
	return false
}

// ytLink is the parsed classification of a youtube link.
type ytLink struct {
	VideoID    string
	PlaylistID string
	// Playlist is true only for playlist-only links. A link carrying a video id is
	// a single-video request even when it also carries list=.
	Playlist bool
}

// parseYTLink classifies a link. ok is false for anything that is not a supported
// youtube video/playlist url.
func parseYTLink(raw string) (ytLink, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ytLink{}, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ytLink{}, false
	}
	if !isYouTubeHost(u.Hostname()) {
		return ytLink{}, false
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	q := u.Query()
	link := ytLink{PlaylistID: q.Get("list")}

	switch host {
	case "youtu.be":
		link.VideoID = strings.Trim(strings.Trim(u.Path, "/"), "/")
	default:
		link.VideoID = q.Get("v")
		if link.VideoID == "" {
			// /shorts/ID, /embed/ID, /live/ID
			for _, p := range []string{"/shorts/", "/embed/", "/live/"} {
				if strings.HasPrefix(u.Path, p) {
					link.VideoID = strings.Trim(strings.TrimPrefix(u.Path, p), "/")
					break
				}
			}
		}
	}

	if link.VideoID != "" {
		link.Playlist = false
		return link, true
	}
	if link.PlaylistID != "" {
		link.Playlist = true
		return link, true
	}
	return ytLink{}, false
}
