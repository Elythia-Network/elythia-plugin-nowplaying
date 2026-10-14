package nowplaying

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/peercache"
)

/*
 * リモート利用者の再生中を、相手のインスタンスから取り寄せる。
 *
 * 経路は mk-go 同士に閉じた peer channel (mk-go #2537)。**問い合わせは非同期**
 * なので、初めて開いたときは出ない。
 */

// peerRequest is what we ask another instance.
//
// **username だけを送る。** 誰が見に来たかは送らない (相手に渡す必要が無い)。
type peerRequest struct {
	Username string `json:"username"`
}

// peerResponse is what the other instance answers.
type peerResponse struct {
	Linked  bool            `json:"linked"`
	Profile json.RawMessage `json:"profile,omitempty"`
}

// remoteTTL / remoteNegativeTTL は peercache に渡す寿命。
//
// 再生中は数分で変わるので肯定側は短い。否定側 (相手が連携していない) は、
// そのたびに問い合わせないよう長めにする。
const (
	remoteTTL         = 2 * time.Minute
	remoteNegativeTTL = 10 * time.Minute
)

// newRemoteCache builds the view-time cache shared by the peer callback and
// the profile route.
//
// **型は plugin/peercache が持つ (#2820)。** 非同期取り寄せ + TTL + 空振りの
// 記憶 + 初回は空、という形は 3 プラグインに手で書かれていた。
func newRemoteCache(ctx plugin.Context, db *sql.DB) (*peercache.Cache, error) {
	return peercache.New(peercache.Options{
		Context:     ctx,
		DB:          db,
		Request:     func(key string) any { return peerRequest{Username: key} },
		TTL:         remoteTTL,
		NegativeTTL: remoteNegativeTTL,
	})
}

// registerPeer wires both directions of the plugin channel.
//
// **Definition.Peer から呼ぶ (#2819)。** Routes の中で登録すると、ロールを
// 分割した構成で応答が届かない。
func registerPeer(ctx plugin.Context, peer plugin.Peer, db *sql.DB, cl *client) error {
	cache, err := newRemoteCache(ctx, db)
	if err != nil {
		return err
	}

	peer.Handle(func(c context.Context, from string, payload json.RawMessage) (any, error) {
		var req peerRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("リクエストを読めません: %w", err)
		}
		if req.Username == "" {
			return nil, errors.New("username が空です")
		}

		userID, err := localUserIDByUsername(c, ctx, req.Username)
		if err != nil {
			return nil, err
		}
		if userID == "" {
			return peerResponse{Linked: false}, nil
		}

		profile, err := buildProfile(c, db, cl, userID)
		if err != nil {
			return nil, err
		}
		if profile == nil {
			return peerResponse{Linked: false}, nil
		}
		body, err := json.Marshal(profile)
		if err != nil {
			return nil, err
		}
		return peerResponse{Linked: true, Profile: body}, nil
	})

	peer.OnReply(func(c context.Context, _, id string, reply json.RawMessage) error {
		var res peerResponse
		if err := json.Unmarshal(reply, &res); err != nil {
			return fmt.Errorf("応答を読めません: %w", err)
		}
		return cache.Store(c, id, res.Profile, res.Linked && len(res.Profile) > 0)
	})
	return nil
}

// localUserIDByUsername resolves a local username to its user id.
//
// **mk-go の API を通す。** 凍結や可視性の判断を自前で実装しないため。
// ここは匿名でよい (ゲートが効くのはリモート利用者を引くときだけ、#2106)。
func localUserIDByUsername(c context.Context, ctx plugin.Context, username string) (string, error) {
	api := ctx.API()
	if api == nil {
		return "", nil
	}
	raw, err := api.Anonymous().Call(c, "users/show", map[string]any{"username": username})
	if err != nil {
		var ae *plugin.APIError
		if errors.As(err, &ae) && ae.Status == 404 {
			return "", nil
		}
		return "", err
	}
	var user struct {
		ID   string  `json:"id"`
		Host *string `json:"host"`
	}
	if err := json.Unmarshal(raw, &user); err != nil {
		return "", err
	}
	// **自分のところの利用者だけ答える。** 又貸しすると出どころが分からなくなる。
	if user.Host != nil && *user.Host != "" {
		return "", nil
	}
	return user.ID, nil
}

// remoteLookup answers for a user that is not ours.
func remoteLookup(c context.Context, ctx plugin.Context, db *sql.DB, viewerID, userID string) (any, error) {
	host, username, err := remoteAcct(c, ctx, viewerID, userID)
	if err != nil || host == "" {
		// **エラーにしない。** 音楽と関係のない利用者を開いただけかもしれない。
		return map[string]any{"linked": false}, nil
	}

	cache, err := newRemoteCache(ctx, db)
	if err != nil {
		return nil, err
	}
	// 初回は空で返り、取り寄せは裏で走る。期限切れでも古いものは返る。
	cached, err := cache.Lookup(c, host, username)
	if err != nil {
		return nil, err
	}
	if len(cached) == 0 {
		return map[string]any{"linked": false}, nil
	}

	profile, ok := sanitizeRemoteProfile(cached)
	if !ok {
		return map[string]any{"linked": false}, nil
	}
	return profile, nil
}

// remoteProfile is the shape a peer's profile answer must have.
type remoteProfile struct {
	Linked    bool    `json:"linked"`
	Service   string  `json:"service"`
	Username  string  `json:"username"`
	Playing   *track  `json:"playing"`
	Recent    []track `json:"recent"`
	FetchedAt string  `json:"fetchedAt"`
}

// maxRemoteRecent bounds the listens taken from a peer.
const maxRemoteRecent = 50

// sanitizeRemoteProfile decodes a peer's answer into the profile shape and
// drops what we must not pass on.
//
// **相手の応答は信用しない (plugin/peer.go)。** 以前は受け取った JSON をそのまま
// 画面へ返し、アートの URL だけを貼り替えていた。相手は自分のサーバーを持つ
// 利用者なので、`url` に `javascript:` や任意のページを、`art` に外部の画像を
// 入れられた (プロフィールを見た人の IP と時刻が相手へ届く)。型に読み直して
// 知らない欄を落とし、`url` は http(s) の絶対 URL だけ、`art` は自分のプロキシの
// 形 (`/api/plugin/nowplaying/art/<鍵>`) だけを通す。
func sanitizeRemoteProfile(cached []byte) (map[string]any, bool) {
	var p remoteProfile
	if err := json.Unmarshal(cached, &p); err != nil || !p.Linked {
		return nil, false
	}
	if p.Playing != nil {
		t := sanitizeTrack(*p.Playing)
		p.Playing = &t
	}
	if len(p.Recent) > maxRemoteRecent {
		p.Recent = p.Recent[:maxRemoteRecent]
	}
	recent := make([]track, 0, len(p.Recent))
	for _, t := range p.Recent {
		recent = append(recent, sanitizeTrack(t))
	}
	return map[string]any{
		"linked":    true,
		"service":   p.Service,
		"username":  p.Username,
		"playing":   p.Playing,
		"recent":    recent,
		"fetchedAt": p.FetchedAt,
	}, true
}

// sanitizeTrack keeps an http(s) page URL and an art URL of our own proxy.
func sanitizeTrack(t track) track {
	t.URL = safePageURL(t.URL)
	// 相手が返したアート URL は**相手のインスタンスの**プロキシを指す。
	// そのまま出すと CSP で表示できないうえ、閲覧者の接続先が相手に漏れる。
	t.Art = rewriteArtURL(t.Art)
	return t
}

// safePageURL returns raw when it is an absolute http(s) URL with a host, and
// "" otherwise.
func safePageURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return raw
}

// remoteAcct resolves a user id to its host and username.
//
// **閲覧者として引く。** 匿名で引くと `ugcVisibilityForVisitor` が `local`
// (既定) のインスタンスでリモート利用者が NO_SUCH_USER になる (mk-go #2106)。
func remoteAcct(c context.Context, ctx plugin.Context, viewerID, userID string) (host, username string, err error) {
	api := ctx.API()
	if api == nil {
		return "", "", nil
	}
	caller := api.Anonymous()
	if viewerID != "" {
		caller = api.AsUser(viewerID)
	}
	raw, err := caller.Call(c, "users/show", map[string]any{"userId": userID})
	if err != nil {
		return "", "", err
	}
	var user struct {
		Username string  `json:"username"`
		Host     *string `json:"host"`
	}
	if err := json.Unmarshal(raw, &user); err != nil {
		return "", "", err
	}
	if user.Host == nil || *user.Host == "" {
		return "", "", nil
	}
	return *user.Host, user.Username, nil
}

// rewriteArtURL keeps only the key part of a peer's proxy URL.
//
// **相手の URL をそのまま使わない。** 鍵だけを取り出して自分の URL に組み直す
// (鍵の形は検証されるので、任意の取得先には化けない)。プロキシの形でないもの
// (外部の画像の URL など) は空にする。以前はそのまま返しており、閲覧者の
// ブラウザが相手の決めた URL の画像を取りに行った。
func rewriteArtURL(s string) string {
	i := strings.Index(s, artRoutePrefix)
	if i < 0 {
		return ""
	}
	key := s[i+len(artRoutePrefix):]
	if !validArtKey(key) {
		return ""
	}
	return artRoutePrefix + key
}

// validArtKey reports whether a proxy key is one we would fetch.
func validArtKey(key string) bool {
	if name, ok := strings.CutPrefix(key, "mb:"); ok {
		return mbidPattern.MatchString(name)
	}
	if name, ok := strings.CutPrefix(key, "lf:"); ok {
		return lastFmImagePattern.MatchString(name)
	}
	return false
}
