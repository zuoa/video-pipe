// Package config holds environment-driven application configuration.
package config

import (
	"fmt"
	"os"
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
}

// Load reads configuration from environment variables, applying defaults.
func Load() (Config, error) {
	c := Config{
		Addr:         env("ADDR", ":8080"),
		DBPath:       env("DB_PATH", "/data/video-pipe.db"),
		MediaMTXAPI:  strings.TrimRight(env("MEDIAMTX_API", "http://mediamtx:9997"), "/"),
		MediaMTXUser: env("MEDIAMTX_USER", "wrapper"),
		MediaMTXPass: env("MEDIAMTX_PASS", "change-me"),
		MediaMTXHost: env("MEDIAMTX_HOST", "mediamtx"),
		PlaybackHost: env("PLAYBACK_HOST", "localhost"),
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
