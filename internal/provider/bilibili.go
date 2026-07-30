package provider

// Hand-written Bilibili resolver (pure Go, stdlib only). An earlier version used
// github.com/synctv-org/vendors, but that drags the kratos framework + gRPC into
// the binary (~40MB). This implementation reimplements the small surface we need
// — WBI signing, the video playurl API, and the live getRoomPlayInfo API — with
// zero non-stdlib dependencies. Reference: SocialSisterYi/bilibili-API-collect.

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type bilibiliResolver struct{}

var (
	biliBVRe   = regexp.MustCompile(`(?i)(BV[0-9a-z]+)`)
	biliAVRe   = regexp.MustCompile(`(?i)av(\d+)`)
	biliLiveRe = regexp.MustCompile(`live\.bilibili\.com/(?:h5/)?(\d+)`)
)

func (b *bilibiliResolver) Resolve(ctx context.Context, pageURL string) (*Result, error) {
	switch {
	case biliLiveRe.MatchString(pageURL):
		return b.resolveLive(ctx, biliLiveRe.FindStringSubmatch(pageURL)[1])
	case biliBVRe.MatchString(pageURL):
		return b.resolveVideo(ctx, "bvid", biliBVRe.FindStringSubmatch(pageURL)[1])
	case biliAVRe.MatchString(pageURL):
		return b.resolveVideo(ctx, "aid", biliAVRe.FindStringSubmatch(pageURL)[1])
	default:
		return nil, fmt.Errorf("bilibili: not a BV/av video or live room url")
	}
}

// resolveVideo fetches a VOD progressive (durl) URL — a single URL ffmpeg can pull.
func (b *bilibiliResolver) resolveVideo(ctx context.Context, idKey, id string) (*Result, error) {
	hdrs := headers("https://www.bilibili.com")

	// 1) Resolve cid from the view API.
	viewURL := "https://api.bilibili.com/x/web-interface/view?" + idKey + "=" + id
	var view struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Bvid string `json:"bvid"`
			Cid  uint64 `json:"cid"`
		} `json:"data"`
	}
	if err := getJSON(ctx, viewURL, hdrs, &view); err != nil {
		return nil, fmt.Errorf("bilibili: view api: %w", err)
	}
	if view.Code != 0 {
		return nil, fmt.Errorf("bilibili: view api code %d: %s", view.Code, view.Msg)
	}

	// 2) Get the playurl with WBI signing.
	mk, err := b.mixinKey(ctx, hdrs)
	if err != nil {
		return nil, fmt.Errorf("bilibili: wbi nav: %w", err)
	}
	params := map[string]string{
		"bvid":  firstNonEmpty(view.Data.Bvid, conditional(idKey == "bvid", id, "")),
		"cid":   strconv.FormatUint(view.Data.Cid, 10),
		"fnval": "1", // progressive durl (single URL); avoids DASH dual-input
		"qn":    "80",
	}
	playURL := "https://api.bilibili.com/x/player/wbi/playurl?" + signWBI(params, mk)
	var play struct {
		Code int `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			DURL []struct {
				URL string `json:"url"`
			} `json:"durl"`
		} `json:"data"`
	}
	if err := getJSON(ctx, playURL, hdrs, &play); err != nil {
		return nil, fmt.Errorf("bilibili: playurl api: %w", err)
	}
	if play.Code != 0 {
		return nil, fmt.Errorf("bilibili: playurl code %d: %s", play.Code, play.Msg)
	}
	for _, d := range play.Data.DURL {
		if d.URL != "" {
			return &Result{URL: d.URL, Headers: hdrs, Live: false}, nil
		}
	}
	return nil, fmt.Errorf("bilibili: no progressive url (try a different video / quality needs login)")
}

// resolveLive fetches a live room's stream URL (FLV/HLS) from getRoomPlayInfo.
func (b *bilibiliResolver) resolveLive(ctx context.Context, room string) (*Result, error) {
	hdrs := headers("https://live.bilibili.com")
	api := "https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo" +
		"?room_id=" + room + "&protocol=2,3&format=0,1,2&codec=0,1&qn=10000"
	var info struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			PlayInfo struct {
				PlayurlStream []struct {
					Format []struct {
						Codec []struct {
							BaseURL string `json:"base_url"`
							URLInfo []struct {
								Host string `json:"host"`
							} `json:"url_info"`
						} `json:"codec"`
					} `json:"format"`
				} `json:"playurl_stream"`
			} `json:"playurl_info"`
		} `json:"data"`
	}
	if err := getJSON(ctx, api, hdrs, &info); err != nil {
		return nil, fmt.Errorf("bilibili: live api: %w", err)
	}
	if info.Code != 0 {
		return nil, fmt.Errorf("bilibili: live code %d: %s", info.Code, info.Msg)
	}
	for _, s := range info.Data.PlayInfo.PlayurlStream {
		for _, f := range s.Format {
			for _, c := range f.Codec {
				if c.BaseURL != "" && len(c.URLInfo) > 0 && c.URLInfo[0].Host != "" {
					return &Result{URL: c.URLInfo[0].Host + c.BaseURL, Headers: hdrs, Live: true}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("bilibili: no live stream url (room may be offline)")
}

// --- WBI signing (https://socialsisteryi.github.io/bilibili-API-collect/docs/misc/sign/wbi.html) ---

var mixinKeyEncTab = []int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 36, 25, 24, 30, 48, 51, 40,
	1, 44, 20, 56, 4, 57, 21, 7, 11, 26, 55, 6, 17, 16, 0, 52, 34, 22, 61, 59, 60,
	63, 62,
}

// mixinKey fetches the WBI img/sub keys from the nav API and derives the mixin key.
func (b *bilibiliResolver) mixinKey(ctx context.Context, hdrs map[string]string) (string, error) {
	var nav struct {
		Code int `json:"code"`
		Data struct {
			WbiImg struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := getJSON(ctx, "https://api.bilibili.com/x/web-interface/nav", hdrs, &nav); err != nil {
		return "", err
	}
	orig := wbiKeyFromURL(nav.Data.WbiImg.ImgURL) + wbiKeyFromURL(nav.Data.WbiImg.SubURL)
	var sb strings.Builder
	for _, i := range mixinKeyEncTab {
		if i < len(orig) {
			sb.WriteByte(orig[i])
		}
	}
	s := sb.String()
	if len(s) > 32 {
		s = s[:32]
	}
	if s == "" {
		return "", fmt.Errorf("empty wbi keys")
	}
	return s, nil
}

func wbiKeyFromURL(u string) string {
	base := path.Base(u)
	if i := strings.LastIndex(base, "."); i >= 0 {
		base = base[:i]
	}
	return base
}

// signWBI adds wts, sorts, encodes, and appends the md5-based w_rid.
func signWBI(params map[string]string, mixinKey string) string {
	params["wts"] = strconv.FormatInt(time.Now().Unix(), 10)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(wbiFilter(k)))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(wbiFilter(params[k])))
	}
	query := b.String()
	sum := md5.Sum([]byte(query + mixinKey))
	return query + "&w_rid=" + hex.EncodeToString(sum[:])
}

// wbiFilter strips characters Bilibili excludes from signed params.
func wbiFilter(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune("!'()*\"", r) {
			return -1
		}
		return r
	}, s)
}

func conditional(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
