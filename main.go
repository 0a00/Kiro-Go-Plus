// Package main provides the entry point for Kiro API Proxy.
//
// Kiro API Proxy is a reverse proxy service that translates Kiro API requests
// into OpenAI and Anthropic (Claude) compatible formats. Key features include:
//   - Multi-account pool with round-robin load balancing
//   - Automatic OAuth token refresh
//   - Streaming response support for real-time AI interactions
//   - Admin panel for account and configuration management
//
// The service exposes the following endpoints:
//   - /v1/messages - Claude API compatible endpoint
//   - /v1/chat/completions - OpenAI API compatible endpoint
//   - /admin - Web-based administration panel
package main

import (
	"context"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/internal/instancelock"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/proxy"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	// 配置文件路径，支持环境变量覆盖
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	// 确保数据目录存在
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}
	dataLock, err := instancelock.Acquire(filepath.Dir(configPath))
	if err != nil {
		log.Fatalf("Failed to lock data directory: %v", err)
	}
	defer dataLock.Close()

	// 加载配置
	if err := config.Init(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize log level: LOG_LEVEL env var takes priority over config, defaulting to "info".
	logger.Init(config.GetLogLevel())
	listenHost, listenPort, _, err := config.ResolveListenAddress()
	if err != nil {
		log.Fatalf("Invalid listen address: %v", err)
	}

	// 环境变量覆盖密码
	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		if err := config.SetPassword(envPassword); err != nil {
			log.Fatalf("Failed to configure admin password: %v", err)
		}
	}
	if host := listenHost; host == "0.0.0.0" || host == "::" || host == "[::]" {
		if config.IsDefaultPassword() {
			allowInsecure := strings.EqualFold(strings.TrimSpace(os.Getenv("ALLOW_INSECURE_PUBLIC_BIND")), "true")
			if !allowInsecure {
				log.Fatal("Refusing public bind with default admin password; set ADMIN_PASSWORD or ALLOW_INSECURE_PUBLIC_BIND=true")
			}
			logger.Warnf("Security warning: insecure public bind explicitly enabled with the default admin password")
		}
		if !config.IsApiKeyRequired() {
			if strings.EqualFold(strings.TrimSpace(os.Getenv("ALLOW_UNAUTHENTICATED_API")), "true") {
				logger.Warnf("Security warning: anonymous public API access explicitly enabled")
			} else {
				logger.Infof("Public API is fail-closed until API-key authentication is enabled; set ALLOW_UNAUTHENTICATED_API=true only for intentional anonymous access")
			}
		}
	}

	// 初始化账号池
	pool.GetPool()

	// 创建 HTTP 处理器（包含后台刷新任务）
	handler := proxy.NewHandler()

	// 启动服务器
	addr := fmt.Sprintf("%s:%d", listenHost, listenPort)
	logger.Infof("Kiro-Go Plus starting on http://%s (log level: %s)", addr, logger.LevelName(logger.GetLevel()))
	logger.Infof("Admin panel: http://%s/admin", addr)
	logger.Infof("Claude API: http://%s/v1/messages", addr)
	logger.Infof("OpenAI API: http://%s/v1/chat/completions", addr)

	// WriteTimeout intentionally 0: SSE streams can run for minutes while the
	// upstream model produces tokens. ReadHeaderTimeout + ReadTimeout still
	// guard against slowloris-style header/body stalls.
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	select {
	case err := <-serverErr:
		handler.Close()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("Server failed: %v", err)
		}
		return
	case sig := <-stop:
		logger.Infof("Received %s, shutting down", sig)
	}

	// Stop background work first, then allow active HTTP streams to finish.
	handler.StopBackground()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Errorf("Graceful shutdown timed out: %v", err)
	}
	handler.Close()
}
