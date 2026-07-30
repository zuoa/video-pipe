// Package config holds environment-driven application configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the resolved application configuration.
type Config struct {
	// Addr is the HTTP listen address for the management API + UI.
	Addr string
	// DBPath is the SQLite database file path.
	DBPath string
	// MediaMTXAPI is the base URL of the MediaMTX Control API (e.g. http://mediamtx:9997).
	MediaMTXAPI string
	// MediaMTXUser / MediaMTXPass are Basic Auth credentials for the Control API.
	MediaMTXUser string
	MediaMTXPass string
	// MediaMTXHost is the host:port the ffmpeg processes push RTSP to (container-internal).
	MediaMTXHost string
	// PlaybackHost is the externally reachable host used to build playback URLs for browsers.
	PlaybackHost string
	// UploadDir stores user-uploaded source files (under the mounted /data volume).
	UploadDir string
	// UploadMaxBytes caps the size of a single upload; 0 means unlimited.
	UploadMaxBytes int64
	// Enabled maps protocol name (rtsp/rtmp/hls/webrtc/srt) to whether it is
	// advertised in the UI/API. Driven by ENABLE_<PROTO> env vars.
	Enabled map[string]bool
}

// Load reads configuration from environment variables, applying defaults.
func Load() (Config, error) {
	c := Config{
		Addr:           env("ADDR", ":8080"),
		DBPath:         env("DB_PATH", "/data/video-pipe.db"),
		MediaMTXAPI:    strings.TrimRight(env("MEDIAMTX_API", "http://mediamtx:9997"), "/"),
		MediaMTXUser:   env("MEDIAMTX_USER", "wrapper"),
		MediaMTXPass:   env("MEDIAMTX_PASS", "change-me"),
		MediaMTXHost:   env("MEDIAMTX_HOST", "mediamtx"),
		PlaybackHost:   env("PLAYBACK_HOST", "localhost"),
		UploadDir:      env("UPLOAD_DIR", "/data/uploads"),
		UploadMaxBytes: envInt("UPLOAD_MAX_BYTES", 0),
		Enabled:        enabledProtocols(),
	}
	if c.PlaybackHost == "" {
		return Config{}, fmt.Errorf("PLAYBACK_HOST must not be empty")
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// protocols is the fixed set of output protocols MediaMTX can serve.
var protocols = []string{"rtsp", "rtmp", "hls", "webrtc", "srt"}

// enabledProtocols reads ENABLE_<PROTO> for each protocol; each defaults to true
// and is disabled by "0", "false", "no", or "off" (case-insensitive).
func enabledProtocols() map[string]bool {
	m := make(map[string]bool, len(protocols))
	for _, p := range protocols {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("ENABLE_" + strings.ToUpper(p))))
		m[p] = !(v == "0" || v == "false" || v == "no" || v == "off")
	}
	return m
}

// ProtocolEnabled reports whether protocol p is advertised (false if unknown).
func (c Config) ProtocolEnabled(p string) bool { return c.Enabled[p] }
