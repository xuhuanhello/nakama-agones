package console

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var feishuHookPath = regexp.MustCompile(`^/open-apis/bot/v2/hook/[A-Za-z0-9_-]{16,128}$`)

func validFeishuWebhook(s string) bool {
	if s == "" {
		return true
	}
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.Host == "open.feishu.cn" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == "" && feishuHookPath.MatchString(u.Path)
}
func validFeishuSecret(s string) bool {
	return len(s) <= 256 && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

type feishuSender struct{ client *http.Client }

func newFeishuSender() *feishuSender {
	return &feishuSender{client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxIdleConnsPerHost: 1}}}
}
func feishuSignature(secret string, unix int64) string {
	key := strconv.FormatInt(unix, 10) + "\n" + secret
	h := hmac.New(sha256.New, []byte(key))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
func (s *feishuSender) Send(ctx context.Context, webhook, secret, text string, now time.Time) error {
	failure := errors.New("feishu_delivery_failed")
	if webhook == "" || !validFeishuWebhook(webhook) || !validFeishuSecret(secret) || len(text) > 12000 {
		return failure
	}
	payload := map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
	if secret != "" {
		payload["timestamp"] = strconv.FormatInt(now.Unix(), 10)
		payload["sign"] = feishuSignature(secret, now.Unix())
	}
	body, _ := json.Marshal(payload)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return failure
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(r)
	if err != nil {
		return failure
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return failure
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return failure
	}
	var result struct {
		Code       *int `json:"code"`
		LegacyCode *int `json:"StatusCode"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return failure
	}
	if result.Code != nil && *result.Code == 0 {
		return nil
	}
	if result.Code == nil && result.LegacyCode != nil && *result.LegacyCode == 0 {
		return nil
	}
	return failure
}
