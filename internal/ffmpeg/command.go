// Package ffmpeg builds and supervises per-stream ffmpeg subprocesses that
// remux an input source into an RTSP stream pushed to MediaMTX.
package ffmpeg

import (
	"fmt"

	"video-pipe/internal/model"
)

// BuildArgs constructs the ffmpeg argument vector for a stream.
//
// The output is always RTSP pushed to MediaMTX (rtsp://<mediamtxHost>:8554/<name>),
// favouring lossless copy (`-c:v copy -c:a copy`). Per-source options:
//   - file:  paced with -re so it is sent at real time
//   - rtsp:   TCP transport + a socket read timeout to detect hung cameras
//   - rtmp:   socket read timeout
//   - http:   libavformat reconnect options for transient failures
//   - test:   an FFmpeg lavfi test pattern (no external source needed)
//
// Only the first video and first audio stream are mapped (`-map 0:v:0? -map 0:a:0?`)
// so stray tracks (data/subtitle/extra audio) do not end up in the output SDP
// without an RTP clock rate; the trailing "?" keeps video-only or audio-only
// sources working.
//
// `-progress pipe:1` writes machine-readable key=value blocks to stdout so the
// supervisor can use `progress=continue` as a data-flow heartbeat; with
// `-nostats -loglevel warning` stderr carries only real log/error lines.
func BuildArgs(s model.Stream, mediamtxHost string) []string {
	out := fmt.Sprintf("rtsp://%s:8554/%s", mediamtxHost, s.Name)
	args := []string{"-nostdin", "-nostats", "-hide_banner", "-loglevel", "warning"}

	switch s.SourceType {
	case model.SourceRTSP:
		args = append(args, "-rtsp_transport", "tcp", "-rw_timeout", "5000000", "-i", s.SourceURL)
	case model.SourceRTMP:
		args = append(args, "-rw_timeout", "5000000", "-i", s.SourceURL)
	case model.SourceHTTP:
		args = append(args,
			"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
			"-i", s.SourceURL,
		)
	case model.SourceTest:
		args = append(args, "-re", "-f", "lavfi", "-i", "testsrc=size=1280x720:rate=25")
	default: // file
		args = append(args, "-re", "-i", s.SourceURL)
	}

	// Map exactly the first video and first audio stream. Without -map, ffmpeg
	// copies EVERY track from the source; stray data/subtitle/extra-audio tracks
	// end up in the output SDP with no RTP clock rate, which MediaMTX rejects
	// ("invalid SDP: media N is invalid: clock rate not found"). The trailing "?"
	// makes each map optional, so video-only (e.g. testsrc) or audio-only sources
	// still work.
	args = append(args,
		"-map", "0:v:0?", "-map", "0:a:0?",
		"-c:v", "copy", "-c:a", "copy",
		"-f", "rtsp", "-rtsp_transport", "tcp", out,
		"-progress", "pipe:1",
	)
	return args
}
