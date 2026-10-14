package nowplaying

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
)

// 相手の応答のアートの URL は、すべて自分のプロキシへ貼り替えること。
func TestSanitizeRemoteProfile_RewritesArt(t *testing.T) {
	raw := `{
		"linked": true, "service": "listenbrainz", "username": "eve",
		"playing": {"title": "a", "artist": "b", "art": "https://other.example` + artRoutePrefix + `mb:` + sampleMBID + `"},
		"recent": [
			{"title": "c", "artist": "d", "art": "https://other.example` + artRoutePrefix + `lf:0123456789abcdef0123456789abcdef.png"},
			{"title": "e", "artist": "f", "art": ""}
		]
	}`
	v, ok := sanitizeRemoteProfile([]byte(raw))
	if !ok {
		t.Fatal("linked な応答を捨てた")
	}
	out, _ := json.Marshal(v)
	s := string(out)
	if strings.Contains(s, "other.example") {
		t.Errorf("相手のホストが残っている: %s", s)
	}
	if !strings.Contains(s, artRoutePrefix+"mb:"+sampleMBID) {
		t.Errorf("再生中のアートが貼り替わっていない: %s", s)
	}
	if !strings.Contains(s, artRoutePrefix+"lf:") {
		t.Errorf("履歴のアートが貼り替わっていない: %s", s)
	}
}

// 相手の応答の `url` は http(s) の絶対 URL だけ、`art` は自分のプロキシの形だけを
// 通すこと。知らない欄は落とすこと。
//
// 相手は自分のサーバーを持つ利用者なので、`url` に `javascript:` を入れると
// プロフィールの「最近」のリンクになり、`art` に外部の画像を入れると見た人の IP が
// 相手へ届いた。
func TestSanitizeRemoteProfile_DropsHostileValues(t *testing.T) {
	raw := `{
		"linked": true, "service": "lastfm", "username": "eve", "extra": "<script>",
		"playing": {"title": "now", "artist": "x", "url": "javascript:alert(1)", "art": "https://tracker.example/pixel.png"},
		"recent": [
			{"title": "a", "artist": "x", "url": "JavaScript:alert(1)"},
			{"title": "b", "artist": "x", "url": "data:text/html,hi"},
			{"title": "c", "artist": "x", "url": "//no-scheme.example/p"},
			{"title": "d", "artist": "x", "url": "https://www.last.fm/music/x", "art": "http://evil.example/` + strings.TrimPrefix(artRoutePrefix, "/") + `not-a-key"}
		]
	}`
	v, ok := sanitizeRemoteProfile([]byte(raw))
	if !ok {
		t.Fatal("linked な応答を捨てた")
	}
	out, _ := json.Marshal(v)
	s := string(out)
	for _, bad := range []string{"javascript", "JavaScript", "data:", "no-scheme", "tracker.example", "evil.example", "extra", "<script>"} {
		if strings.Contains(s, bad) {
			t.Errorf("%q が残っている: %s", bad, s)
		}
	}
	if !strings.Contains(s, "https://www.last.fm/music/x") {
		t.Errorf("正しいページの URL まで落とした: %s", s)
	}
	if _, ok := sanitizeRemoteProfile([]byte(`{"linked": false}`)); ok {
		t.Error("linked でない応答を通した")
	}
}

// fakeAPI records which caller the plugin used.
type fakeAPI struct {
	calls []string
	resp  json.RawMessage
	err   error
}

func (a *fakeAPI) Anonymous() plugin.Caller { return &fakeCaller{api: a, who: "anonymous"} }
func (a *fakeAPI) AsUser(userID string) plugin.Caller {
	return &fakeCaller{api: a, who: "asUser:" + userID}
}

type fakeCaller struct {
	api *fakeAPI
	who string
}

func (c *fakeCaller) Call(_ context.Context, endpoint string, _ any) (json.RawMessage, error) {
	c.api.calls = append(c.api.calls, c.who+" "+endpoint)
	return c.api.resp, c.api.err
}

type fakeCtx struct {
	plugin.Context
	api plugin.API
}

func (c *fakeCtx) API() plugin.API { return c.api }

// **閲覧者として引く。** 匿名で引くと ugcVisibilityForVisitor='local' (既定) の
// インスタンスでリモート利用者が NO_SUCH_USER になり、問い合わせ自体が出せない
// (mk-go #2106)。原神版で実際にこれを踏んだ。
func TestRemoteAcct_CallsAsViewer(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":"other.example"}`)}
	host, username, err := remoteAcct(context.Background(), &fakeCtx{api: api}, "viewer1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if host != "other.example" || username != "alice" {
		t.Fatalf("host/username: %q / %q", host, username)
	}
	if len(api.calls) != 1 || api.calls[0] != "asUser:viewer1 users/show" {
		t.Errorf("閲覧者として呼んでいない: %v", api.calls)
	}
}

func TestRemoteAcct_AnonymousWhenNoViewer(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":"other.example"}`)}
	if _, _, err := remoteAcct(context.Background(), &fakeCtx{api: api}, "", "u1"); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 1 || api.calls[0] != "anonymous users/show" {
		t.Errorf("匿名で呼んでいない: %v", api.calls)
	}
}

// ローカル利用者には host が無い。問い合わせ先が無いので何もしない。
func TestRemoteAcct_LocalUser(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":null}`)}
	host, _, err := remoteAcct(context.Background(), &fakeCtx{api: api}, "viewer1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		t.Errorf("ローカル利用者に host を返した: %q", host)
	}
}

func TestRemoteAcct_NoAPI(t *testing.T) {
	host, _, err := remoteAcct(context.Background(), &fakeCtx{}, "viewer1", "u1")
	if err != nil || host != "" {
		t.Errorf("host=%q err=%v", host, err)
	}
}

func TestLocalUserIDByUsername(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"id":"u1","host":null}`)}
	got, err := localUserIDByUsername(context.Background(), &fakeCtx{api: api}, "alice")
	if err != nil || got != "u1" {
		t.Fatalf("id=%q err=%v", got, err)
	}

	// **他所の利用者は答えない。** 又貸しすると出どころが分からなくなる。
	remote := &fakeAPI{resp: json.RawMessage(`{"id":"u2","host":"third.example"}`)}
	if got, _ := localUserIDByUsername(context.Background(), &fakeCtx{api: remote}, "bob"); got != "" {
		t.Errorf("又貸ししている: %q", got)
	}

	missing := &fakeAPI{err: &plugin.APIError{Status: 404}}
	got, err = localUserIDByUsername(context.Background(), &fakeCtx{api: missing}, "nobody")
	if err != nil || got != "" {
		t.Fatalf("404 をエラーにしている: %q / %v", got, err)
	}

	if got, _ := localUserIDByUsername(context.Background(), &fakeCtx{}, "alice"); got != "" {
		t.Error("API 未配線で値を返している")
	}
}

// --- peer channel ---

func peerHarness(t *testing.T, api plugin.API) (*plugintest.Harness, plugintest.Handlers) {
	t.Helper()
	srv := lbServer(t, lbNowPlaying, lbListens)
	h := plugintest.New(t).
		WithName("nowplaying").
		WithDB(testDB(t)).
		WithAPI(api).
		WithPeers("other.example").
		WithConfig(testConfig(srv.URL, ""))
	// **Peer の登録は Definition.Peer 経由 (mk-go #2819 / #2820)。**
	h.Peer(Plugin)
	return h, h.Routes(Plugin)
}

// 相手から聞かれたら、自分のところの利用者の分だけ答える。
func TestPeer_AnswersForLocalUser(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"id":"u1","host":null}`)}
	h, routes := peerHarness(t, api)

	if _, err := routes.Call(t, "POST /me/set", plugintest.Request{
		UserID: "u1", Body: `{"service":"listenbrainz","username":"someone"}`,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := h.DeliverPeer("other.example", peerRequest{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	got := res.(peerResponse)
	if !got.Linked {
		t.Fatal("登録済みの利用者を linked=false で返している")
	}
	if !strings.Contains(string(got.Profile), "What Else Is There?") {
		t.Errorf("再生中が入っていない: %s", got.Profile)
	}
}

func TestPeer_AnswersUnlinked(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"id":"u9","host":null}`)}
	h, _ := peerHarness(t, api)

	res, err := h.DeliverPeer("other.example", peerRequest{Username: "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if res.(peerResponse).Linked {
		t.Error("登録していない利用者を linked=true で返している")
	}
}

// **中身は信用しない。** 相手は同じプラグインを持っているだけで善良とは限らない。
func TestPeer_RejectsBadRequest(t *testing.T) {
	h, _ := peerHarness(t, &fakeAPI{resp: json.RawMessage(`{"id":"u1","host":null}`)})

	if _, err := h.DeliverPeer("other.example", map[string]any{"username": ""}); err == nil {
		t.Error("空の username を通している")
	}
	if _, err := h.DeliverPeer("other.example", "not an object"); err == nil {
		t.Error("読めない payload を通している")
	}
}

// 問い合わせは非同期。初回は「まだ無い」を返しつつ、裏で送ること。
func TestRemoteLookup_AsksThenServesCache(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":"other.example"}`)}
	h, routes := peerHarness(t, api)

	res, err := routes.Call(t, "POST /profile", plugintest.Request{UserID: "viewer1", Body: `{"userId":"u-remote"}`})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["linked"] != false {
		t.Errorf("初回から値を返している: %+v", res)
	}
	sends := h.PeerSends()
	if len(sends) != 1 || sends[0].Host != "other.example" {
		t.Fatalf("問い合わせを出していない: %+v", sends)
	}

	profile := map[string]any{
		"linked":   true,
		"service":  "listenbrainz",
		"username": "alice",
		"playing": map[string]any{
			"title": "Roygbiv", "artist": "Boards of Canada",
			"art": "https://other.example" + artRoutePrefix + "mb:" + sampleMBID,
		},
	}
	body, _ := json.Marshal(profile)
	if err := h.DeliverPeerReply("other.example", sends[0].ID, peerResponse{Linked: true, Profile: body}); err != nil {
		t.Fatal(err)
	}

	res, err = routes.Call(t, "POST /profile", plugintest.Request{UserID: "viewer1", Body: `{"userId":"u-remote"}`})
	if err != nil {
		t.Fatal(err)
	}
	// 画面に出る形 (JSON) で見る。
	encoded, _ := json.Marshal(res)
	var m map[string]any
	if err := json.Unmarshal(encoded, &m); err != nil {
		t.Fatal(err)
	}
	playing, _ := m["playing"].(map[string]any)
	if playing == nil || playing["title"] != "Roygbiv" {
		t.Fatalf("取り寄せた分が出ていない: %+v", m)
	}
	// 相手のホストが残っていないこと。
	if art, _ := playing["art"].(string); art != artRoutePrefix+"mb:"+sampleMBID {
		t.Errorf("アートが貼り替わっていない: %q", art)
	}
}

// 相手が同じプラグインを持っていなければ黙って諦める。
func TestRemoteLookup_SkipsUnknownPeer(t *testing.T) {
	api := &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":"stranger.example"}`)}
	h, routes := peerHarness(t, api)

	res, err := routes.Call(t, "POST /profile", plugintest.Request{UserID: "viewer1", Body: `{"userId":"u-remote"}`})
	if err != nil {
		t.Fatalf("エラーにするべきではない: %v", err)
	}
	if res.(map[string]any)["linked"] != false {
		t.Errorf("値を返している: %+v", res)
	}
	if len(h.PeerSends()) != 0 {
		t.Error("持っていない相手に送っている")
	}
}

// どの問い合わせの答えか分からない応答は捨てる。
func TestPeer_DropsUncorrelatedReply(t *testing.T) {
	h, routes := peerHarness(t, &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":"other.example"}`)})

	body, _ := json.Marshal(map[string]any{"linked": true, "username": "誰か"})
	if err := h.DeliverPeerReply("other.example", "unknown-id", peerResponse{Linked: true, Profile: body}); err != nil {
		t.Fatal(err)
	}

	res, err := routes.Call(t, "POST /profile", plugintest.Request{UserID: "viewer1", Body: `{"userId":"u-remote"}`})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["username"] == "誰か" {
		t.Error("相関の取れない応答を取り込んでいる")
	}
}

func TestPeer_RejectsBadReply(t *testing.T) {
	h, _ := peerHarness(t, &fakeAPI{resp: json.RawMessage(`{"username":"alice","host":"other.example"}`)})

	if err := h.DeliverPeerReply("other.example", "x", "not an object"); err == nil {
		t.Error("読めない応答を通している")
	}
}
