package provider

import "testing"

func TestSelectBiliLiveURL(t *testing.T) {
	streams := []biliLiveStream{
		{
			ProtocolName: "http_hls",
			Format: []biliLiveFormat{{
				FormatName: "ts",
				Codec: []biliLiveCodec{{
					CodecName: "avc",
					BaseURL:   "/fallback.m3u8",
					URLInfo:   []biliLiveURLInfo{{Host: "https://hls.example", Extra: "?token=fallback"}},
				}},
			}},
		},
		{
			ProtocolName: "http_stream",
			Format: []biliLiveFormat{{
				FormatName: "flv",
				Codec: []biliLiveCodec{{
					CodecName: "avc",
					BaseURL:   "/live.flv",
					URLInfo:   []biliLiveURLInfo{{Host: "https://cdn.example", Extra: "?token=signed"}},
				}},
			}},
		},
	}

	got := selectBiliLiveURL(streams)
	want := "https://cdn.example/live.flv?token=signed"
	if got != want {
		t.Fatalf("selectBiliLiveURL() = %q, want %q", got, want)
	}
}

func TestGet_KnownProviders(t *testing.T) {
	for _, p := range []string{"bilibili", "douyu"} {
		if _, ok := Get(p); !ok {
			t.Errorf("Get(%q) = false, want true", p)
		}
	}
	if _, ok := Get("nope"); ok {
		t.Error(`Get("nope") = true, want false`)
	}
}

func TestDouyuRoomFromURL(t *testing.T) {
	cases := map[string]string{
		"https://www.douyu.com/12345":        "12345",
		"https://www.douyu.com/12345?isHD=1": "12345",
		"https://www.douyu.com/9252212":      "9252212",
		"https://m.douyu.com/9252212":        "9252212",
		"not a url":                          "",
	}
	for in, want := range cases {
		if got := douyuRoomFromURL(in); got != want {
			t.Errorf("douyuRoomFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDouyuRealRoomID(t *testing.T) {
	html := `<script>$ROOM.room_id = 9252212;</script>`
	if got := douyuRealRoomID(html, "fallback"); got != "9252212" {
		t.Errorf("douyuRealRoomID = %q, want 9252212", got)
	}
	if got := douyuRealRoomID("<html></html>", "123"); got != "123" {
		t.Errorf("douyuRealRoomID fallback = %q, want 123", got)
	}
}

func TestDouyuSign_ExtractsHex(t *testing.T) {
	// A self-contained signer: the script block holds the var the function uses
	// and returns a 32-hex sign (real ub98484234 is obfuscated but same shape).
	page := `<html><script>var vdwdae3xw=[1,2,3];
		function ub98484234(room, did, tt){
			var s = vdwdae3xw.join("") ;
			return "deadbeefdeadbeefdeadbeefdeadbeef&did=" + did + "&tt=" + tt + s;
		}</script></html>`
	sign, err := douyuSign(page, "9252212", "abc123def456", "1700000000")
	if err != nil {
		t.Fatalf("douyuSign: unexpected error: %v", err)
	}
	if want := "deadbeefdeadbeefdeadbeefdeadbeef"; sign != want {
		t.Errorf("douyuSign = %q, want %q", sign, want)
	}
}

func TestDouyuSign_MissingFunction(t *testing.T) {
	if _, err := douyuSign("<html><body>no script here</body></html>", "1", "d", "t"); err == nil {
		t.Fatal("douyuSign: expected error when ub98484234 is absent")
	}
}
