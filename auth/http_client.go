// Package auth 提供认证相关功能的 HTTP 客户端
package auth

import (
	"context"
	"fmt"
	"kiro-go/config"
	"kiro-go/internal/clientcache"
	"kiro-go/internal/outboundipv6"
	"kiro-go/internal/outboundproxy"
	"kiro-go/logger"
	"net"
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

func GetAuthClientForAccount(account *config.Account) (*http.Client, error) {
	if account == nil {
		return httpClient(), nil
	}
	accountProxyURL := strings.TrimSpace(account.ProxyURL)
	proxyURL := accountProxyURL
	if proxyURL == "" {
		proxyURL = config.GetProxyURL()
	}
	mode, _, err := outboundproxy.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if mode != outboundproxy.Direct {
		return GetAuthClientForProxy(proxyURL)
	}
	value := config.GetOutboundIPv6Config()
	addr, err := outboundipv6.Address(value, account.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve account IPv6: %w", err)
	}
	if !addr.IsValid() {
		if accountProxyURL == "" {
			return httpClient(), nil
		}
		return GetAuthClientForProxy(proxyURL)
	}
	cacheKey := proxyURL + "|ipv6:" + addr.String()
	if value.FallbackEnabled {
		cacheKey += "|fallback"
	}
	transport, err := buildAuthTransportWithIPv6(proxyURL, addr.String(), value.FallbackEnabled)
	if err != nil {
		return nil, err
	}
	return authProxyClientCache.Get(cacheKey, func() *http.Client {
		return &http.Client{Timeout: 30 * time.Second, Transport: transport}
	}), nil
}

// buildAuthTransport 构建带可选代理的 Transport
func buildAuthTransport(proxyURL string) (*http.Transport, error) {
	return buildAuthTransportWithIPv6(proxyURL, "", false)
}

func buildAuthTransportWithIPv6(proxyURL, sourceIPv6 string, fallback bool) (*http.Transport, error) {
	var dialContext func(context.Context, string, string) (net.Conn, error)
	if sourceIPv6 != "" {
		ip := net.ParseIP(sourceIPv6)
		if ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("invalid source IPv6 address")
		}
		bound := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, LocalAddr: &net.TCPAddr{IP: ip}}
		dialContext = bound.DialContext
		if fallback {
			plain := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
			dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := bound.DialContext(ctx, network, address)
				if err == nil {
					return conn, nil
				}
				logger.Warnf("Auth IPv6 source bind %s failed, falling back to default route: %v", sourceIPv6, err)
				return plain.DialContext(ctx, network, address)
			}
		}
	}
	t := &http.Transport{
		DialContext:         dialContext,
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
