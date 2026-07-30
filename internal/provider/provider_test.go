package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

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
	nextData := `roomInfo\":{\"room\":{\"room_id\":9252212,\"vipId\":0}`
	if got := douyuRealRoomID(nextData, "fallback"); got != "9252212" {
		t.Errorf("douyuRealRoomID Next data = %q, want 9252212", got)
	}
	if got := douyuRealRoomID("<html></html>", "123"); got != "123" {
		t.Errorf("douyuRealRoomID fallback = %q, want 123", got)
	}
}

func TestDouyuStreamAuth(t *testing.T) {
	data := douyuEncryptionData{
		Key:     "secret",
		RandStr: "random",
		EncTime: 2,
		EncData: "opaque",
	}
	auth, err := douyuStreamAuth(data, "9252212", "1700000000")
	if err != nil {
		t.Fatalf("douyuStreamAuth: unexpected error: %v", err)
	}
	want := md5Hex(md5Hex(md5Hex("randomsecret")+"secret") + "secret92522121700000000")
	if auth != want {
		t.Errorf("douyuStreamAuth = %q, want %q", auth, want)
	}
}

func TestDouyuPlayResponse_StringData(t *testing.T) {
	var failed douyuPlayResponse
	if err := json.Unmarshal([]byte(`{"error":102,"msg":"room is offline","data":""}`), &failed); err != nil {
		t.Fatalf("unmarshal failure response: %v", err)
	}
	if failed.Error != 102 || failed.Msg != "room is offline" {
		t.Fatalf("failure response = code %d, msg %q", failed.Error, failed.Msg)
	}
	if data, err := failed.playData(); err != nil || data != (douyuPlayData{}) {
		t.Fatalf("empty string data = %#v, %v", data, err)
	}

	var encoded douyuPlayResponse
	body := `{"error":0,"msg":"ok","data":"{\"rtmp_url\":\"https://cdn.example/live\",\"rtmp_live\":\"room.flv\"}"}`
	if err := json.Unmarshal([]byte(body), &encoded); err != nil {
		t.Fatalf("unmarshal encoded response: %v", err)
	}
	data, err := encoded.playData()
	if err != nil {
		t.Fatalf("playData: %v", err)
	}
	if data.RtmpURL != "https://cdn.example/live" || data.RtmpLive != "room.flv" {
		t.Fatalf("playData = %#v", data)
	}
}

func TestDouyuResolver_CurrentWebFlow(t *testing.T) {
	const (
		alias     = "12345"
		roomID    = "9252212"
		key       = "secret"
		randStr   = "random"
		encData   = "opaque-token"
		stream    = "live.flv?token=signed"
		streamCDN = "https://cdn.example/live"
	)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/RoomApi/room/"+alias:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": 0,
				"data":  map[string]any{"room_id": roomID},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/wgapi/livenc/liveweb/websec/getEncryption":
			if r.URL.Query().Get("did") == "" {
				t.Error("getEncryption request has no did")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": 0,
				"msg":   "ok",
				"data": map[string]any{
					"key": key, "rand_str": randStr, "enc_time": 1,
					"expire_at": 4102444800, "is_special": 0, "enc_data": encData,
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/lapi/live/getH5PlayV1/"+roomID:
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assertDouyuPlayForm(t, r.Form, roomID, key, randStr, encData)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": 0,
				"msg":   "ok",
				"data":  map[string]any{"rtmp_url": streamCDN, "rtmp_live": stream},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	resolver := &douyuResolver{origin: server.URL, roomAPIOrigin: server.URL}
	got, err := resolver.Resolve(context.Background(), server.URL+"/"+alias)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := streamCDN + "/" + stream; got.URL != want {
		t.Errorf("Resolve URL = %q, want %q", got.URL, want)
	}
	if !got.Live {
		t.Error("Resolve Live = false, want true")
	}
	if got.Headers["Referer"] != server.URL+"/"+alias {
		t.Errorf("Resolve Referer = %q", got.Headers["Referer"])
	}
}

func assertDouyuPlayForm(t *testing.T, form url.Values, roomID, key, randStr, encData string) {
	t.Helper()
	tt := form.Get("tt")
	if tt == "" {
		t.Error("getH5PlayV1 request has no tt")
	}
	if form.Get("did") == "" {
		t.Error("getH5PlayV1 request has no did")
	}
	wantAuth := md5Hex(md5Hex(randStr+key) + key + roomID + tt)
	checks := map[string]string{
		"enc_data": encData,
		"auth":     wantAuth,
		"ver":      "Douyu_new",
		"rate":     "-1",
		"hevc":     "0",
		"fa":       "0",
		"ive":      "0",
	}
	for key, want := range checks {
		if got := form.Get(key); got != want {
			t.Errorf("getH5PlayV1 form %s = %q, want %q", key, got, want)
		}
	}
	if got := form.Encode(); !strings.Contains(got, "cdn=") {
		t.Errorf("getH5PlayV1 form is missing empty cdn: %q", got)
	}
}
