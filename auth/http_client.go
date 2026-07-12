// Package auth 提供认证相关功能的 HTTP 客户端
package auth

import (
	"kiro-go/internal/clientcache"
	"kiro-go/internal/outboundproxy"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// 全局 HTTP 客户端存储，支持运行时代理重配置
var httpClientStore atomic.Pointer[http.Client]

// authProxyClientCache caches per-proxy auth HTTP clients.
var authProxyClientCache = clientcache.New(1024, 30*time.Minute)

// httpClient 返回当前全局 auth HTTP 客户端
func httpClient() *http.Client {
	return httpClientStore.Load()
}

func init() {
	if err := InitHttpClient(""); err != nil {
		panic(err)
	}
}

// GetAuthClientForProxy returns an auth HTTP client for the given proxy URL.
// If proxyURL is empty, returns the global auth HTTP client.
func GetAuthClientForProxy(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return httpClient(), nil
	}
	transport, err := buildAuthTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	return authProxyClientCache.Get(proxyURL, func() *http.Client {
		return &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
	}), nil
}

// buildAuthTransport 构建带可选代理的 Transport
func buildAuthTransport(proxyURL string) (*http.Transport, error) {
	t := &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}
	if err := outboundproxy.Apply(t, proxyURL); err != nil {
		return nil, err
	}
	return t, nil
}

// InitHttpClient 初始化（或重新初始化）auth 模块的全局 HTTP 客户端

func InitHttpClient(proxyURL string) error {
	transport, err := buildAuthTransport(proxyURL)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
	httpClientStore.Store(client)
	return nil
}
