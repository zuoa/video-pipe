// Package ffmpeg builds and supervises per-stream ffmpeg subprocesses that
// remux an input source into an RTSP stream pushed to MediaMTX.
package ffmpeg

import (
	"fmt"
	"strings"

	"video-pipe/internal/model"
)

// BuildArgs constructs the ffmpeg argument vector for a stream.
//
// The output is always RTSP pushed to MediaMTX (rtsp://<mediamtxHost>:8554/<name>).
// Direct sources are remuxed losslessly (`-c:v copy -c:a copy`). Provider
// sources are normalized to H.264 Baseline without B-frames + Opus since their
// source codecs are not guaranteed to be browser-compatible. The `test` source
// is encoded to the same WebRTC-safe H.264 profile because its lavfi input is
// rawvideo, which cannot be copy-muxed over RTSP. Per-source options (used only
// when resURL is empty):
//   - file:  paced with -re so it is sent at real time
//   - rtsp:   TCP transport + a socket read timeout to detect hung cameras
//   - rtmp:   socket read timeout
//   - http:   libavformat reconnect options for transient failures
//   - test:   a hue-cycling lavfi test pattern (no external source needed), encoded to H.264
//
// When resURL is non-empty, it is used instead of s.SourceURL. HTTP(S) provider
// URLs get the supplied headers and reconnect options. A local resolved path is
// paced and looped forever; this is used by cached Bilibili VODs.
//
// For real sources only the first video and first audio stream are mapped
// (`-map 0:v:0? -map 0:a:0?`) so stray tracks (data/subtitle/extra audio) do not
// end up in the output SDP without an RTP clock rate; the trailing "?" keeps
// video-only or audio-only sources working.
//
// `-progress pipe:1` writes machine-readable key=value blocks to stdout so the
// supervisor can use `progress=continue` as a data-flow heartbeat; with
// `-nostats -loglevel warning` stderr carries only real log/error lines.
func BuildArgs(s model.Stream, mediamtxHost, resURL string, headers map[string]string) []string {
	out := fmt.Sprintf("rtsp://%s:8554/%s", mediamtxHost, s.Name)
	args := []string{"-nostdin", "-nostats", "-hide_banner", "-loglevel", "warning"}

	switch {
	case isHTTPURL(resURL):
		// Provider-resolved source: http URL with site-specific headers.
		args = append(args, "-user_agent", headerUserAgent(headers))
		if h := headerBlock(headers); h != "" {
			args = append(args, "-headers", h)
		}
		args = append(args,
			"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
			"-i", resURL,
		)
	case resURL != "":
		// Downloaded provider VOD: do not start until the complete local file
		// exists, then pace it at media time and loop in one ffmpeg process.
		args = append(args, "-stream_loop", "-1", "-re", "-i", resURL)
	case s.SourceType == model.SourceRTSP:
		args = append(args, "-rtsp_transport", "tcp", "-rw_timeout", "5000000", "-i", s.SourceURL)
	case s.SourceType == model.SourceRTMP:
		args = append(args, "-rw_timeout", "5000000", "-i", s.SourceURL)
	case s.SourceType == model.SourceHTTP:
		args = append(args,
			"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
			"-i", s.SourceURL,
		)
	case s.SourceType == model.SourceTest:
		// lavfi test pattern whose hue rotates over time (full color cycle ~8s at
		// 45 deg/s), so the picture is visibly "alive" in a player.
		args = append(args, "-re", "-f", "lavfi", "-i", "testsrc=size=1280x720:rate=25,hue=h=45*t")
	default: // file
		args = append(args, "-re", "-i", s.SourceURL)
	}

	// Codec selection. Direct sources are remuxed losslessly. Provider media is
	// normalized because H.264 with B-frames (common in VOD files) and AAC audio
	// cannot be consumed by WebRTC browsers. Baseline + zerolatency + bf=0 makes
	// the video WebRTC-safe; Opus works in both MediaMTX WebRTC and fMP4 HLS.
	codecs := []string{"-map", "0:v:0?", "-map", "0:a:0?", "-c:v", "copy", "-c:a", "copy"}
	if s.Provider != "" {
		codecs = []string{
			"-map", "0:v:0?", "-map", "0:a:0?",
			"-c:v", "libx264", "-profile:v", "baseline", "-pix_fmt", "yuv420p",
			"-preset", "veryfast", "-tune", "zerolatency", "-g", "50", "-bf", "0",
			"-c:a", "libopus", "-b:a", "96k", "-ar", "48000",
		}
	} else if s.SourceType == model.SourceTest {
		codecs = []string{
			"-c:v", "libx264", "-profile:v", "baseline", "-pix_fmt", "yuv420p",
			"-preset", "veryfast", "-tune", "zerolatency", "-g", "25", "-bf", "0", "-an",
		}
	}
	args = append(args, codecs...)
	args = append(args, "-f", "rtsp", "-rtsp_transport", "tcp", out, "-progress", "pipe:1")
	return args
}

func isHTTPURL(rawURL string) bool {
	return strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://")
}

// headerUserAgent returns the User-Agent from headers (case-insensitive) or a
// generic fallback.
func headerUserAgent(headers map[string]string) string {
	for k, v := range headers {
		if strings.EqualFold(k, "User-Agent") {
			return v
		}
	}
	return "Mozilla/5.0"
}

// headerBlock formats all non-User-Agent headers as an ffmpeg -headers string
// ("Key: Value\r\n" per line); returns "" when there are none.
func headerBlock(headers map[string]string) string {
	var b strings.Builder
	for k, v := range headers {
		if strings.EqualFold(k, "User-Agent") {
			continue
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\r\n")
	}
	return b.String()
}
