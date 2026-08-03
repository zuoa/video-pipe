package provider

import (
	"net/url"
	"strings"
)

// DetectFromURL returns the provider name for a page/room URL ("bilibili",
// "douyu"), or "" if the URL doesn't belong to a known provider. It lets the
// "auto" source type do what users expect: pasting a Bilibili video page or a
// Douyu room URL just works, instead of feeding the HTML page to ffmpeg.
//
// Matching is by host suffix, so CDN media URLs (e.g. *.bilivideo.com) are not
// misclassified — those are direct sources ffmpeg can pull as-is.
func DetectFromURL(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	if !strings.Contains(s, "://") {
		s = "https://" + s // tolerate scheme-less "bilibili.com/video/BV..."
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "bilibili.com" || strings.HasSuffix(host, ".bilibili.com"):
		return "bilibili"
	case host == "douyu.com" || strings.HasSuffix(host, ".douyu.com"):
		return "douyu"
	default:
		return ""
	}
}
