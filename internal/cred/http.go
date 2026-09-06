package cred

import (
	"bytes"
	"io"
	"net/http"
	"time"
)

// httpPostJSON 最小依赖的 JSON POST，返回原始响应体与状态码。
// 单独抽出来是为了让 cred 包不依赖 upstream（避免循环引用）。
func httpPostJSON(url string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}
