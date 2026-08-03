package provider

import "testing"

func TestDetectFromURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://www.bilibili.com/video/BV1Ey4y1M7cM", "bilibili"},
		{"bilibili.com/video/BV1Pu57z3EmN/?spm_id_from=333.337", "bilibili"},
		{"https://live.bilibili.com/12345", "bilibili"},
		{"https://www.douyu.com/37777", "douyu"},
		{"https://www.douyu.com/topic/xx?rid=37777", "douyu"},
		// Direct CDN media URLs must stay direct — ffmpeg pulls them as-is.
		{"https://upos-sz-mirrorali.bilivideo.com/video/123.m4s", ""},
		{"https://example.com/live/index.m3u8", ""},
		{"rtsp://camera.local:554/stream1", ""},
		{"", ""},
		{"not a url", ""},
	}
	for _, c := range cases {
		if got := DetectFromURL(c.url); got != c.want {
			t.Errorf("DetectFromURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
