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
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := BuildArgs(tc.s, "mediamtx", "", nil)
			joined := strings.Join(args, " ")
			checks := map[string]bool{
				"has -loglevel warning": containsPair(args, "-loglevel", "warning"),
				"copies video":          containsPair(args, "-c:v", "copy"),
				"copies audio":          containsPair(args, "-c:a", "copy"),
				"outputs rtsp":          containsPair(args, "-f", "rtsp"),
				"has progress pipe":     containsPair(args, "-progress", "pipe:1"),
				"target path present":   strings.Contains(joined, "rtsp://mediamtx:8554/cam1"),
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
	args := BuildArgs(stream(model.SourceFile, "/data/a.mp4"), "mediamtx", "", nil)
	if !contains(args, "-re") {
		t.Errorf("file source must pace with -re; got %v", args)
	}
}

func TestBuildArgs_LiveHasNoRealtimeFlag(t *testing.T) {
	args := BuildArgs(stream(model.SourceRTSP, "rtsp://x"), "mediamtx", "", nil)
	if contains(args, "-re") {
		t.Errorf("live RTSP source should not set -re; got %v", args)
	}
}

func TestBuildArgs_CachedProviderVODLoopsLocally(t *testing.T) {
	const local = "/data/provider-cache/bili.media"
	s := stream(model.SourceHTTP, "https://www.bilibili.com/video/BV1xx")
	s.Provider = "bilibili"
	args := BuildArgs(s, "mediamtx", local, nil)

	if !containsPair(args, "-stream_loop", "-1") {
		t.Errorf("cached provider VOD must loop forever; got %v", args)
	}
	if !contains(args, "-re") {
		t.Errorf("cached provider VOD must be paced with -re; got %v", args)
	}
	if !containsPair(args, "-i", local) {
		t.Errorf("cached provider VOD must use local input; got %v", args)
	}
	for _, forbidden := range []string{"-reconnect", "-user_agent", "-headers"} {
		if contains(args, forbidden) {
			t.Errorf("cached provider VOD must not use HTTP option %s; got %v", forbidden, args)
		}
	}
	for label, ok := range map[string]bool{
		"encodes H264":        containsPair(args, "-c:v", "libx264"),
		"uses baseline":       containsPair(args, "-profile:v", "baseline"),
		"disables B-frames":   containsPair(args, "-bf", "0"),
		"encodes Opus":        containsPair(args, "-c:a", "libopus"),
		"does not copy video": !containsPair(args, "-c:v", "copy"),
	} {
		if !ok {
			t.Errorf("provider compatibility: %s; got %v", label, args)
		}
	}
}

func TestBuildArgs_RTSPTransportBeforeInput(t *testing.T) {
	args := BuildArgs(stream(model.SourceRTSP, "rtsp://cam/stream"), "mediamtx", "", nil)
	i := indexOf(args, "-i")
	rt := indexOf(args, "-rtsp_transport")
	if rt == -1 || rt > i || i == -1 {
		t.Errorf("-rtsp_transport must precede -i for an RTSP input; got %v", args)
	}
}

func TestBuildArgs_MapsSingleVideoAndAudio(t *testing.T) {
	args := BuildArgs(stream(model.SourceRTSP, "rtsp://cam/stream"), "mediamtx", "", nil)
	if !containsPair(args, "-map", "0:v:0?") {
		t.Errorf("must optionally map first video (-map 0:v:0?); got %v", args)
	}
	if !containsPair(args, "-map", "0:a:0?") {
		t.Errorf("must optionally map first audio (-map 0:a:0?); got %v", args)
	}
	// -map must sit after the input (-i) and before the codecs (-c:v).
	i := indexOf(args, "-i")
	m := indexOf(args, "-map")
	cv := indexOf(args, "-c:v")
	if i == -1 || m == -1 || cv == -1 || !(i < m && m < cv) {
		t.Errorf("expected -i < -map < -c:v; got i=%d map=%d cv=%d args=%v", i, m, cv, args)
	}
}

func TestBuildArgs_PublishesToStreamName(t *testing.T) {
	s := stream(model.SourceTest, "")
	s.Name = "random"
	args := BuildArgs(s, "mediamtx", "", nil)
	if !strings.Contains(strings.Join(args, " "), "rtsp://mediamtx:8554/random") {
		t.Fatalf("output path missing random: %v", args)
	}
}

func TestBuildArgs_TestSourceEncodesToH264(t *testing.T) {
	args := BuildArgs(stream(model.SourceTest, ""), "mediamtx", "", nil)
	joined := strings.Join(args, " ")
	checks := map[string]bool{
		"encodes video to h264": containsPair(args, "-c:v", "libx264"),
		"uses baseline profile": containsPair(args, "-profile:v", "baseline"),
		"disables B-frames":     containsPair(args, "-bf", "0"),
		"no audio (-an)":        contains(args, "-an"),
		"does not copy video":   !containsPair(args, "-c:v", "copy"),
		"does not copy audio":   !containsPair(args, "-c:a", "copy"),
		"hue cycles over time":  strings.Contains(joined, "hue=h=45*t"),
		"target path present":   strings.Contains(joined, "rtsp://mediamtx:8554/cam1"),
	}
	for label, ok := range checks {
		if !ok {
			t.Errorf("test source: %s\n  args: %v", label, args)
		}
	}
}

func contains(args []string, v string) bool { return indexOf(args, v) >= 0 }
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
