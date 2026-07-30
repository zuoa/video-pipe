package ffmpeg

import "testing"

func TestCleanExitStops(t *testing.T) {
	tests := []struct {
		name     string
		live     bool
		resolved bool
		want     bool
	}{
		{name: "uploaded file stops", live: false, resolved: false, want: true},
		{name: "provider VOD replays", live: false, resolved: true, want: false},
		{name: "direct live source retries", live: true, resolved: false, want: false},
		{name: "provider live source retries", live: true, resolved: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanExitStops(tt.live, tt.resolved); got != tt.want {
				t.Fatalf("cleanExitStops(%v, %v) = %v, want %v", tt.live, tt.resolved, got, tt.want)
			}
		})
	}
}
