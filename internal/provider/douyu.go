package provider

// EXPERIMENTAL. Douyu live stream resolution is self-implemented (there is no
// clean importable Go library for it). It requires Douyu's `getH5Play` *sign*,
// computed by the page's obfuscated `ub98484234` JS function. We extract that
// function from the room page and evaluate it with goja (a pure-Go JS engine)
// so we don't have to re-derive the algorithm ourselves.
//
// Because Douyu changes the page/algorithm periodically, the regexes below are
// the single point most likely to need adjustment; validate against a real room.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
)

type douyuResolver struct{}

func (d *douyuResolver) Resolve(ctx context.Context, pageURL string) (*Result, error) {
	room := douyuRoomFromURL(pageURL)
	if room == "" {
		return nil, fmt.Errorf("douyu: could not read a room id from %q", pageURL)
	}
	referer := "https://www.douyu.com/" + room
	hdrs := headers(referer)

	did := randomDid()
	tt := strconv.FormatInt(time.Now().Unix(), 10)

	// 1) Fetch the room page HTML (it contains the real room id + ub98484234).
	html, err := fetchText(ctx, "https://www.douyu.com/"+room, hdrs)
	if err != nil {
		return nil, fmt.Errorf("douyu: fetch room page: %w", err)
	}
	realRoom := douyuRealRoomID(html, room)

	// 2) Compute the sign by evaluating the page's ub98484234 function.
	sign, err := douyuSign(html, realRoom, did, tt)
	if err != nil {
		return nil, fmt.Errorf("douyu: compute sign: %w", err)
	}

	// 3) Call getH5Play to get the CDN FLV url.
	var resp struct {
		Data struct {
			RtmpURL  string `json:"rtmp_url"`
			RtmpLive string `json:"rtmp_live"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	form := url.Values{}
	form.Set("rate", "0")
	form.Set("tt", tt)
	form.Set("did", did)
	form.Set("sign", sign)
	apiURL := "https://www.douyu.com/lapi/live/getH5Play/" + realRoom

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	hresp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("douyu: getH5Play: %w", err)
	}
	defer hresp.Body.Close()
	if err := decodeBody(hresp, &resp); err != nil {
		return nil, fmt.Errorf("douyu: decode getH5Play: %w", err)
	}
	u := resp.Data.RtmpURL + resp.Data.RtmpLive
	if u == "" {
		return nil, fmt.Errorf("douyu: no stream url (room may be offline): %s", resp.Msg)
	}
	return &Result{URL: u, Headers: hdrs, Live: true}, nil
}

// douyuSign evaluates the page's obfuscated ub98484234(roomID, did, tt) function
// in a goja VM and extracts the 32-hex sign from its return value.
func douyuSign(pageHTML, roomID, did, tt string) (string, error) {
	src := extractUB98484234(pageHTML)
	if src == "" {
		return "", errors.New("ub98484234 function not found in room page (Douyu layout changed?)")
	}
	vm := goja.New()
	if _, err := vm.RunString(src); err != nil {
		return "", fmt.Errorf("eval ub98484234: %w", err)
	}
	fnVal := vm.Get("ub98484234")
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		return "", errors.New("ub98484234 is not callable")
	}
	ret, err := fn(goja.Undefined(), vm.ToValue(roomID), vm.ToValue(did), vm.ToValue(tt))
	if err != nil {
		return "", fmt.Errorf("call ub98484234: %w", err)
	}
	out := ret.String()
	if m := signRe.FindString(out); m != "" {
		return m, nil
	}
	return "", fmt.Errorf("no 32-hex sign in ub98484234 output %q", out)
}

var (
	// room id or alias is the last numeric path segment of a douyu URL.
	douyuRoomRe = regexp.MustCompile(`(\d+)/?(?:[\?#].*)?$`)
	// real room id embedded in the page (Douyu short aliases resolve to these).
	douyuRealRoomRe = regexp.MustCompile(`(?:\$ROOM\.room_id|"room_id"|\$ROOM\['room_id'\]|data-rid)[^\d]{0,3}(\d{3,})`)
	// any <script>…</script> block, used to locate the signer function + its deps.
	scriptRe = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
	// the sign is an MD5 (32 hex chars).
	signRe = regexp.MustCompile(`[0-9a-f]{32}`)
)

// extractUB98484234 returns the first <script> block containing the signer
// function (it also holds the variables ub98484234 depends on). The whole block
// is evaluated by goja; if Douyu's page layout changes, this is the adjustment point.
func extractUB98484234(pageHTML string) string {
	for _, m := range scriptRe.FindAllStringSubmatch(pageHTML, -1) {
		if strings.Contains(m[1], "ub98484234") {
			return m[1]
		}
	}
	return ""
}

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
