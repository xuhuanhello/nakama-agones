package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type feishuRoundTrip func(*http.Request) (*http.Response, error)

func (f feishuRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFeishuSignatureAndRequestUseOfficialEmptyMessageHMAC(t *testing.T) {
	// Independent Python hmac.new(b"1700000000\nfixture-secret", b"", sha256).
	if got := feishuSignature("fixture-secret", 1700000000); got != "eSSBpPPdWfBl7avPl9BWSBfQOnLGZ91Xn8G5oZis0sc=" {
		t.Fatalf("signature mismatch: %s", got)
	}
	sender := newFeishuSender()
	requests := 0
	sender.client.Transport = feishuRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "POST" || r.URL.String() != fakeWebhook || r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("unexpected outgoing request")
		}
		var value map[string]any
		if json.NewDecoder(r.Body).Decode(&value) != nil {
			t.Fatal("invalid JSON")
		}
		if value["timestamp"] != "1700000000" || value["sign"] != feishuSignature("fixture-secret", 1700000000) || value["msg_type"] != "text" {
			t.Fatal("wrong signed payload")
		}
		if strings.Contains(value["content"].(map[string]any)["text"].(string), "fixture-secret") {
			t.Fatal("secret in content")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":0}`)), Header: make(http.Header), Request: r}, nil
	})
	if err := sender.Send(context.Background(), fakeWebhook, "fixture-secret", "Synthetic fixture only", time.Unix(1700000000, 0)); err != nil || requests != 1 {
		t.Fatal("signed fixture rejected")
	}
}

func TestFeishuNeverFollowsRedirectOrEchoesProviderError(t *testing.T) {
	for _, item := range []struct {
		status int
		body   string
	}{
		{302, `redirect-secret`}, {403, `private-access-denied`}, {200, `{"code":1,"msg":"private-provider-detail"}`}, {200, `{}`}, {200, strings.Repeat("x", 65537)},
	} {
		sender := newFeishuSender()
		calls := 0
		sender.client.Transport = feishuRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: item.status, Body: io.NopCloser(strings.NewReader(item.body)), Header: http.Header{"Location": []string{"http://127.0.0.1/private"}}, Request: r}, nil
		})
		err := sender.Send(context.Background(), fakeWebhook, "", "fixture", time.Unix(1700000000, 0))
		if err == nil || err.Error() != "feishu_delivery_failed" || calls != 1 {
			t.Fatal("redirect/error boundary violated")
		}
	}
	transport := newFeishuSender().client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("ambient proxy enabled")
	}
}
