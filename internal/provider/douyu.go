package provider

// EXPERIMENTAL. Douyu live stream resolution is self-implemented (there is no
// clean importable Go library for it). Douyu's current web player first obtains
// a short-lived encryption key, computes an MD5-based auth value, and then calls
// getH5PlayV1. Keep this flow aligned with the web player when Douyu changes it.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const douyuOrigin = "https://www.douyu.com"

type douyuResolver struct {
	// origin is overridden by tests. Production uses douyuOrigin.
	origin string
}

func (d *douyuResolver) Resolve(ctx context.Context, pageURL string) (*Result, error) {
	room := douyuRoomFromURL(pageURL)
	if room == "" {
		return nil, fmt.Errorf("douyu: could not read a room id from %q", pageURL)
	}
	origin := d.origin
	if origin == "" {
		origin = douyuOrigin
	}
	origin = strings.TrimRight(origin, "/")
	referer := origin + "/" + room
	hdrs := headers(referer)

	did := randomDid()
	tt := strconv.FormatInt(time.Now().Unix(), 10)

	// Fetch the room page to resolve numeric aliases to the canonical room id.
	html, err := fetchText(ctx, referer, hdrs)
	if err != nil {
		return nil, fmt.Errorf("douyu: fetch room page: %w", err)
	}
	realRoom := douyuRealRoomID(html, room)

	// The encryption response is tied to the device id and user agent, so all
	// requests in this flow intentionally use the same values.
	encryptionURL := origin + "/wgapi/livenc/liveweb/websec/getEncryption?did=" + url.QueryEscape(did)
	var encryption douyuEncryptionResponse
	if err := getJSON(ctx, encryptionURL, hdrs, &encryption); err != nil {
		return nil, fmt.Errorf("douyu: get encryption key: %w", err)
	}
	if encryption.Error != 0 {
		return nil, fmt.Errorf("douyu: get encryption key: code %d: %s", encryption.Error, encryption.Msg)
	}
	auth, err := douyuStreamAuth(encryption.Data, realRoom, tt)
	if err != nil {
		return nil, fmt.Errorf("douyu: compute stream auth: %w", err)
	}

	// Call the current web-player endpoint to get the signed CDN FLV URL.
	form := url.Values{}
	form.Set("enc_data", encryption.Data.EncData)
	form.Set("tt", tt)
	form.Set("did", did)
	form.Set("auth", auth)
	form.Set("cdn", "")
	form.Set("ver", "Douyu_new")
	form.Set("rate", "-1")
	form.Set("hevc", "0")
	form.Set("fa", "0")
	form.Set("ive", "0")
	apiURL := origin + "/lapi/live/getH5PlayV1/" + realRoom

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	hresp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("douyu: getH5PlayV1: %w", err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("douyu: getH5PlayV1: http %d", hresp.StatusCode)
	}
	var resp douyuPlayResponse
	if err := decodeBody(hresp, &resp); err != nil {
		return nil, fmt.Errorf("douyu: decode getH5PlayV1: %w", err)
	}
	if resp.Error != 0 {
		return nil, fmt.Errorf("douyu: getH5PlayV1: code %d: %s", resp.Error, resp.Msg)
	}
	play, err := resp.playData()
	if err != nil {
		return nil, fmt.Errorf("douyu: decode getH5PlayV1 data: %w", err)
	}
	u := play.Player1
	if u == "" && play.RtmpURL != "" && play.RtmpLive != "" {
		u = strings.TrimRight(play.RtmpURL, "/") + "/" + strings.TrimLeft(play.RtmpLive, "/")
	}
	if u == "" {
		return nil, fmt.Errorf("douyu: no stream url (room may be offline): %s", resp.Msg)
	}
	return &Result{URL: u, Headers: hdrs, Live: true}, nil
}

type douyuEncryptionResponse struct {
	Error int                 `json:"error"`
	Msg   string              `json:"msg"`
	Data  douyuEncryptionData `json:"data"`
}

type douyuEncryptionData struct {
	Key       string `json:"key"`
	RandStr   string `json:"rand_str"`
	EncTime   int    `json:"enc_time"`
	ExpireAt  int64  `json:"expire_at"`
	IsSpecial int    `json:"is_special"`
	EncData   string `json:"enc_data"`
}

type douyuPlayResponse struct {
	Error int             `json:"error"`
	Msg   string          `json:"msg"`
	Data  json.RawMessage `json:"data"`
}

type douyuPlayData struct {
	RtmpURL  string `json:"rtmp_url"`
	RtmpLive string `json:"rtmp_live"`
	Player1  string `json:"player_1"`
}

// playData accepts both response shapes seen in the wild. Successful responses
// normally contain an object, while failures commonly use data:"". Some Douyu
// edges also return a JSON-encoded object or a direct URL as a string.
func (r douyuPlayResponse) playData() (douyuPlayData, error) {
	raw := bytes.TrimSpace(r.Data)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return douyuPlayData{}, nil
	}
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return douyuPlayData{}, err
		}
		encoded = strings.TrimSpace(encoded)
		if encoded == "" {
			return douyuPlayData{}, nil
		}
		if strings.HasPrefix(encoded, "http://") || strings.HasPrefix(encoded, "https://") {
			return douyuPlayData{Player1: encoded}, nil
		}
		if !json.Valid([]byte(encoded)) {
			if len(encoded) > 200 {
				encoded = encoded[:200] + "..."
			}
			return douyuPlayData{}, fmt.Errorf("unexpected string response %q", encoded)
		}
		raw = []byte(encoded)
	}
	var data douyuPlayData
	if err := json.Unmarshal(raw, &data); err != nil {
		return douyuPlayData{}, err
	}
	return data, nil
}

// douyuStreamAuth mirrors the current web player's stream-signing algorithm:
// hash rand_str+key enc_time times, then hash key, room id, and timestamp into it.
func douyuStreamAuth(data douyuEncryptionData, roomID, tt string) (string, error) {
	if data.Key == "" || data.RandStr == "" || data.EncData == "" {
		return "", fmt.Errorf("incomplete encryption key response")
	}
	if data.EncTime < 0 || data.EncTime > 1000 {
		return "", fmt.Errorf("invalid encryption iteration count %d", data.EncTime)
	}
	auth := data.RandStr
	for range data.EncTime {
		auth = md5Hex(auth + data.Key)
	}
	suffix := ""
	if data.IsSpecial != 1 {
		suffix = roomID + tt
	}
	return md5Hex(auth + data.Key + suffix), nil
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

var (
	// room id or alias is the last numeric path segment of a douyu URL.
	douyuRoomRe = regexp.MustCompile(`(\d+)/?(?:[\?#].*)?$`)
	// real room id embedded in the page (Douyu short aliases resolve to these).
	douyuRealRoomRe = regexp.MustCompile(`(?:\$ROOM\.room_id|"room_id"|\$ROOM\['room_id'\]|data-rid)[^\d]{0,3}(\d{3,})`)
)

func douyuRoomFromURL(u string) string {
	if m := douyuRoomRe.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return ""
}

func douyuRealRoomID(pageHTML, fallback string) string {
	if m := douyuRealRoomRe.FindStringSubmatch(pageHTML); m != nil {
		return m[1]
	}
	return fallback
}

func randomDid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b) // 32 hex chars — a plausible device id
}
