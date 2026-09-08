package service

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// ResolveWeChatIDByCode exchanges a short-lived, single-use code with the
// configured WeChat server. The server consumes the code even if the caller
// subsequently rejects its identity; callers must not retry an exchange.
// See https://github.com/songquanpeng/wechat-server/blob/master/common/verification.go.
func ResolveWeChatIDByCode(code string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 128 {
		return "", ErrVerificationFailed
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(common.WeChatServerAddress, "/")+"/api/wechat/user?code="+url.QueryEscape(code), nil)
	if err != nil {
		return "", ErrVerificationFailed
	}
	req.Header.Set("Authorization", common.WeChatServerToken)
	client := http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(req)
	if err != nil {
		// Transport errors can include the request URL and its usable code.
		return "", ErrVerificationFailed
	}
	defer response.Body.Close()
	var result struct {
		Success bool   `json:"success"`
		Data    string `json:"data"`
	}
	if response.StatusCode != http.StatusOK || common.DecodeJson(response.Body, &result) != nil || !result.Success || result.Data == "" {
		return "", ErrVerificationFailed
	}
	return result.Data, nil
}
