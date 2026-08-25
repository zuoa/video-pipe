// Package model defines the core domain types for video-pipe.
package model

import (
	"regexp"
	"strings"
	"time"
)

// Source type identifiers. The "auto" type is resolved into a concrete type
// by DeriveSourceType before a stream is persisted.
const (
	SourceAuto = "auto"
	SourceFile = "file"
	SourceRTSP = "rtsp"
	SourceRTMP = "rtmp"
	SourceHTTP = "http"
	SourceTest = "test" // FFmpeg lavfi test pattern, for smoke testing without a real source.
)

// Desired administrative states. "running" means enabled for on-demand
// playback; it does not imply that an ffmpeg process is currently resident.
const (
	StateRunning = "running"
	StateStopped = "stopped"
)

// ValidSourceTypes are the source types a user may select.
var ValidSourceTypes = []string{SourceAuto, SourceFile, SourceRTSP, SourceRTMP, SourceHTTP, SourceTest}

// ValidProviders are the optional source-URL resolvers ("" = direct URL, no resolution).
var ValidProviders = []string{"", "bilibili", "douyu"}

// IsValidProvider reports whether p is a valid (or empty) provider.
func IsValidProvider(p string) bool {
	for _, v := range ValidProviders {
		if v == p {
			return true
		}
	}
	return false
}

// nameRe constrains stream names: they become a MediaMTX path and an ffmpeg
// output URL segment, so they must be URL-safe slugs.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidName reports whether name is an acceptable stream identifier.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// IsValidSourceType reports whether t is a selectable source type.
func IsValidSourceType(t string) bool {
	for _, v := range ValidSourceTypes {
		if v == t {
			return true
		}
	}
	return false
}

// Reader is one consumer of a stream as reported by MediaMTX. Type is the
// MediaMTX reader kind (rtspSession, rtmpConn, webRTCSession, srtConn,
// hlsMuxer). Remote is the consumer's host:port; empty when MediaMTX doesn't
// expose it (HLS readers aggregate behind the muxer).
type Reader struct {
	Type   string `json:"type"`
	Remote string `json:"remote"`
}

// Stream is a single configured input source. The Name doubles as the
// MediaMTX publication path.
type Stream struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	SourceURL    string    `json:"source_url"`
	SourceType   string    `json:"source_type"`
	Provider     string    `json:"provider"` // optional URL resolver: "", "bilibili", "douyu"
	Live         bool      `json:"live"`
	DesiredState string    `json:"desired_state"`
	CreatedAt    time.Time `json:"created_at"`
}

// DeriveSourceType resolves an "auto" source type from the URL scheme; other
// types are returned unchanged.
func DeriveSourceType(sourceType, sourceURL string) string {
	if sourceType != SourceAuto {
		return sourceType
	}
	switch {
	case strings.HasPrefix(sourceURL, "rtsp://"), strings.HasPrefix(sourceURL, "rtsps://"):
		return SourceRTSP
	case strings.HasPrefix(sourceURL, "rtmp://"), strings.HasPrefix(sourceURL, "rtmps://"):
		return SourceRTMP
	case strings.HasPrefix(sourceURL, "http://"), strings.HasPrefix(sourceURL, "https://"):
		return SourceHTTP
	default:
		return SourceFile
	}
}

// IsLive reports whether a source type behaves as a live (never-ending) feed.
// File sources terminate cleanly on EOF and must not be restarted forever.
func IsLive(sourceType string) bool {
	switch sourceType {
	case SourceFile:
		return false
	default:
		return true
	}
}
