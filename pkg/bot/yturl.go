package dbot

import (
	"fmt"
	"net/url"
	"strings"
)

// The single form used for new artefact keys, playlist entries and backup lookups, so
// the same video always resolves to the same row.
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

type ytLink struct {
	VideoID    string
	PlaylistID string
	// a link carrying a video id is a single-video request even with list=
	Playlist bool
}

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
	// only the real playlist route counts; a path that merely carries list= does not
	if link.PlaylistID != "" && strings.TrimSuffix(u.Path, "/") == "/playlist" {
		link.Playlist = true
		return link, true
	}
	return ytLink{}, false
}
