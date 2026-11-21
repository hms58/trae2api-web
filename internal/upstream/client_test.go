package upstream

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"trae2api-web/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{200, `{"code":1005,"message":"plan limit","extra":{"plan":2}}`, ErrPlanLimit},
		{200, `{"code":1005,"msg":"权益不足"}`, ErrPlanLimit},
		{429, ``, ErrSoftRate},
		{401, `{"code":1001,"msg":"login required"}`, ErrSessionDead},
		{401, ``, ErrSessionDead},
		{404, ``, ErrNotFound},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{400, `{"code":11101,"msg":"bad param"}`, ErrClient},
		{200, `{"checked_in":false}`, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:      &http.Client{Transport: fn},
		AgentHost: "https://agent.example",
		UgHost:    "https://ug.example",
		OAuthHost: "https://oauth.example",
		ClientID:  ClientID,
	}
}

func TestRefreshTokenExchange(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpExchange) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			return nil, errors.New("missing content-type")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ClientID":"en1oxy7wnw8j9n"`)) || !bytes.Contains(body, []byte(`"RefreshToken":"oldrt"`)) {
			return nil, errors.New("bad body: " + string(body))
		}
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt != 1786805537 {
		t.Errorf("expiresAt=%d", a.ExpiresAt)
	}
}

// TestRefreshTokenExchangeMilliseconds 覆盖上游 TokenExpireAt 返回毫秒的场景：
// 必须归一化为 Unix 秒后再写 auth.ExpiresAt。
func TestRefreshTokenExchangeMilliseconds(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1786847930 {
		t.Errorf("expiresAt=%d want 1786847930 (毫秒转秒)", a.ExpiresAt)
	}
}

func TestRefreshTokenIfNeededSkipsFresh(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, ApiHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Error("fresh token should not refresh")
	}
	if calls != 0 {
		t.Errorf("ExchangeToken should not be called, calls=%d", calls)
	}
	if a.AccessToken != "at" {
		t.Error("token should remain unchanged")
	}
}

func TestRefreshTokenIfNeededRefreshesExpired(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || calls != 1 {
		t.Errorf("expired token should refresh once, refreshed=%v calls=%d", refreshed, calls)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
}

func TestRefreshTokenUsesAuthApiHost(t *testing.T) {
	var gotHost string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotHost = r.URL.Scheme + "://" + r.URL.Host
		return jsonResp(200, `{"Result":{"Token":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, ApiHost: "https://custom.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
	if gotHost != "https://custom.example" {
		t.Errorf("host=%s want auth.apiHost", gotHost)
	}
}

func TestChatStreamSendsHeadersAndRewritesBody(t *testing.T) {
	var gotAuth, gotUID, gotAppID, gotIdeVer string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-Uid")
		gotAppID = r.Header.Get("X-App-Id")
		gotIdeVer = r.Header.Get("X-Ide-Version")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", MachineID: "m1", DeviceID: "d1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Cloud-IDE-JWT at" || gotUID != "u1" {
		t.Errorf("headers: auth=%q uid=%q", gotAuth, gotUID)
	}
	if gotAppID != AppID || gotIdeVer != IdeVersion {
		t.Errorf("app headers: appid=%q idever=%q", gotAppID, gotIdeVer)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) || !bytes.Contains(gotBody, []byte(`"function":"solo_work_lite"`)) {
		t.Errorf("body not rewritten: %s", gotBody)
	}
}

func TestChatStreamUsesDedicatedStreamClient(t *testing.T) {
	// StreamHTTP 优先于 HTTP 被 ChatStream 使用（无总超时的长 SSE 流客户端）。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	c.StreamHTTP = &http.Client{Transport: c.HTTP.Transport} // 无 Timeout
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if c.StreamHTTP.Timeout != 0 {
		t.Errorf("stream client should have no total timeout, got %v", c.StreamHTTP.Timeout)
	}
}

func TestChatStreamHTTPError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(429, `rate limited`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`))
	if status != 429 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("429 should come via status, err=%v", err)
	}
	if Classify(status, string(respBody)) != ErrSoftRate {
		t.Errorf("not classified soft rate: %q", respBody)
	}
}

func TestUserEntUsageAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpEntUsage) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT at" {
			return nil, errors.New("missing auth header")
		}
		return jsonResp(200, `{"is_credits_billing":true,"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits_limit":2000}}},
			{"entitlement_base_info":{"quota":{"credits_limit":500}}}
		]}`), nil
	})
	remain, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("ent usage: %v", err)
	}
	if remain != 2500 {
		t.Errorf("remain=%d want 2500", remain)
	}
}

func TestCheckinStatusAndClaim(t *testing.T) {
	var path string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		if r.Header.Get("X-User-Region") != "CN" {
			return nil, errors.New("missing X-User-Region")
		}
		if r.URL.Path == EpCheckinClaim {
			return jsonResp(200, `{"code":0,"credits":200,"message":"success"}`), nil
		}
		return jsonResp(200, `{"checked_in":false,"credits":200,"enable":true}`), nil
	})
	checkedIn, credits, enable, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if checkedIn || !enable || credits != 200 {
		t.Errorf("status: checked=%v enable=%v credits=%d", checkedIn, enable, credits)
	}
	if path != EpCheckinStatus {
		t.Errorf("path=%s", path)
	}
	gained, err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if gained != 200 {
		t.Errorf("gained=%d want 200", gained)
	}
	if path != EpCheckinClaim {
		t.Errorf("claim path=%s", path)
	}
}

// 领取接口的业务码必须被解析：9074（设备未登记/风控）以 HTTP 200 + code 返回，
// 旧实现丢弃响应体直接当成功，导致「显示成功其实没领到」。
func TestCheckinClaimBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`), nil
	})
	gained, err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatalf("code=9074 must fail, gained=%d", gained)
	}
	var ce *CheckinError
	if !errors.As(err, &ce) || ce.Code != 9074 {
		t.Fatalf("err=%v want *CheckinError code 9074", err)
	}
	if !IsDeviceRejected(err) {
		t.Error("9074 should be classified as device rejected")
	}
	if IsAlreadyCheckedIn(err) {
		t.Error("9074 is not already-checked-in")
	}
}

// 9095（该设备当日已被其它账号占用）同样必须报错。
func TestCheckinClaimDeviceOccupied(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":9095,"message":"设备今日已被占用"}`), nil
	})
	if _, err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); !IsDeviceRejected(err) {
		t.Errorf("err=%v want device rejected", err)
	}
}

// 领取奖励兼容 data.points / data.credits 两种下发位置。
func TestCheckinClaimCreditsInData(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"data":{"points":150}}`,
		`{"code":0,"data":{"credits":150}}`,
	} {
		c := testClient(func(r *http.Request) (*http.Response, error) {
			return jsonResp(200, body), nil
		})
		gained, err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
		if err != nil || gained != 150 {
			t.Errorf("body=%s gained=%d err=%v", body, gained, err)
		}
	}
}

// data 不是对象时不能把成功响应解析成失败。
func TestCheckinClaimNonObjectData(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"credits":100,"data":"ok"}`), nil
	})
	gained, err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err != nil || gained != 100 {
		t.Errorf("gained=%d err=%v", gained, err)
	}
}

// 状态接口 code != 0（如 1001 令牌失效）必须报错，
// 否则零值会被读成「未签到 + 未开启」，调用方当成无事可做。
func TestCheckinStatusBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":1001,"message":"not able to authenticate you"}`), nil
	})
	_, _, _, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"})
	var ce *CheckinError
	if !errors.As(err, &ce) || ce.Code != 1001 {
		t.Fatalf("err=%v want *CheckinError code 1001", err)
	}
}

// enable 缺省视为开启：上游只在活动关闭时下发 false。
func TestCheckinStatusEnableDefaultsTrue(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"checked_in":false,"credits":100}`), nil
	})
	checkedIn, _, enable, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"})
	if err != nil || checkedIn || !enable {
		t.Errorf("checked=%v enable=%v err=%v", checkedIn, enable, err)
	}
}

func TestCheckinErrorAlready(t *testing.T) {
	if !IsAlreadyCheckedIn(&CheckinError{Code: 1, Message: "今日已签到"}) {
		t.Error("中文已签到应识别为幂等")
	}
	if !IsAlreadyCheckedIn(&CheckinError{Code: 1, Message: "Already checked in today"}) {
		t.Error("英文 already checked 应识别为幂等")
	}
	if IsAlreadyCheckedIn(errors.New("already checked")) {
		t.Error("非业务错误不应被当作已签到")
	}
}
