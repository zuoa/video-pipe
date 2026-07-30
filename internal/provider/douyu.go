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

	// getH5PlayV1 only accepts the canonical room id. Numeric vanity ids and
	// aliases must first be resolved through the same room metadata endpoint
	// used by Douyu's web application.
	realRoom, err := douyuCanonicalRoomID(ctx, origin, referer, room, hdrs)
	if err != nil {
		return nil, fmt.Errorf("douyu: resolve room id: %w", err)
	}

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
		return nil, fmt.Errorf(
			"douyu: getH5PlayV1: code %d for room %s (input %s): %s",
			resp.Error, realRoom, room, resp.Msg,
		)
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

type douyuRoomInfoResponse struct {
	Room struct {
		RoomID json.RawMessage `json:"room_id"`
	} `json:"room"`
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

func douyuCanonicalRoomID(
	ctx context.Context,
	origin, referer, room string,
	hdrs map[string]string,
) (string, error) {
	var info douyuRoomInfoResponse
	infoErr := getJSON(ctx, origin+"/betard/"+url.PathEscape(room), hdrs, &info)
	if infoErr == nil {
		if canonical := numericJSONValue(info.Room.RoomID); canonical != "" {
			return canonical, nil
		}
		infoErr = fmt.Errorf("room metadata has no room_id")
	}

	// Keep a page-data fallback for regional edges where /betard is unavailable.
	html, pageErr := fetchText(ctx, referer, hdrs)
	if pageErr == nil {
		if canonical := douyuRealRoomID(html, ""); canonical != "" {
			return canonical, nil
		}
		pageErr = fmt.Errorf("room page has no canonical room_id")
	}
	return "", fmt.Errorf("metadata: %v; page fallback: %v", infoErr, pageErr)
}

func numericJSONValue(raw json.RawMessage) string {
	value := strings.Trim(string(bytes.TrimSpace(raw)), `"`)
	if value == "" {
		return ""
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return value
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
