package ffmpeg

import (
	"strings"
	"testing"

	"video-pipe/internal/model"
)

func stream(t string, url string) model.Stream {
	return model.Stream{Name: "cam1", SourceURL: url, SourceType: t, Live: model.IsLive(t)}
}

func TestBuildArgs_Common(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    model.Stream
	}{
		{"rtsp", stream(model.SourceRTSP, "rtsp://1.2.3.4/stream")},
		{"rtmp", stream(model.SourceRTMP, "rtmp://host/live/k")},
		{"http", stream(model.SourceHTTP, "https://host/v.m3u8")},
		{"file", stream(model.SourceFile, "/data/a.mp4")},
		{"test", stream(model.SourceTest, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := BuildArgs(tc.s, "mediamtx")
			joined := strings.Join(args, " ")
			checks := map[string]bool{
				"has -loglevel warning":  containsPair(args, "-loglevel", "warning"),
				"copies video":           containsPair(args, "-c:v", "copy"),
				"copies audio":           containsPair(args, "-c:a", "copy"),
				"outputs rtsp":           containsPair(args, "-f", "rtsp"),
				"has progress pipe":      containsPair(args, "-progress", "pipe:1"),
				"target path present":    strings.Contains(joined, "rtsp://mediamtx:8554/cam1"),
			}
			for label, ok := range checks {
				if !ok {
					t.Errorf("%s: %s\n  args: %v", tc.name, label, args)
				}
			}
		})
	}
}

func TestBuildArgs_FileHasRealtimeFlag(t *testing.T) {
	args := BuildArgs(stream(model.SourceFile, "/data/a.mp4"), "mediamtx")
	if !contains(args, "-re") {
		t.Errorf("file source must pace with -re; got %v", args)
	}
}

func TestBuildArgs_LiveHasNoRealtimeFlag(t *testing.T) {
	args := BuildArgs(stream(model.SourceRTSP, "rtsp://x"), "mediamtx")
	if contains(args, "-re") {
		t.Errorf("live RTSP source should not set -re; got %v", args)
	}
}

func TestBuildArgs_RTSPTransportBeforeInput(t *testing.T) {
	args := BuildArgs(stream(model.SourceRTSP, "rtsp://cam/stream"), "mediamtx")
	i := indexOf(args, "-i")
	rt := indexOf(args, "-rtsp_transport")
	if rt == -1 || rt > i || i == -1 {
		t.Errorf("-rtsp_transport must precede -i for an RTSP input; got %v", args)
	}
}

func contains(args []string, v string) bool        { return indexOf(args, v) >= 0 }
func indexOf(args []string, v string) int {
	for i, a := range args {
		if a == v {
			return i
		}
	}
	return -1
}
func containsPair(args []string, k, v string) bool {
	// Scan ALL positions: a flag like -f can legitimately appear twice
	// (e.g. test source uses -f lavfi for input AND -f rtsp for output).
	for i := 0; i+1 < len(args); i++ {
		if args[i] == k && args[i+1] == v {
			return true
		}
	}
	return false
}
