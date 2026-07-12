package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"kiro-go/config"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	adminSessionCookie       = "kiro_admin_session"
	adminSessionDuration     = 12 * time.Hour
	adminRememberDuration    = 72 * time.Hour
	adminAttemptWindow       = 5 * time.Minute
	adminAttemptLockDuration = 5 * time.Minute
	adminMaxAttempts         = 5
	adminMaxSessions         = 1024
	adminMaxAttemptEntries   = 4096
)

type adminSession struct {
	ExpiresAt time.Time
}

type adminAuthAttempt struct {
	WindowStarted time.Time
	Failures      int
	LockedUntil   time.Time
}

func adminClientKey(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	if remote := strings.TrimSpace(r.RemoteAddr); remote != "" {
		return remote
	}
	return "unknown"
}

func (h *Handler) adminAttemptAllowed(r *http.Request) (bool, time.Duration) {
	now := time.Now()
	key := adminClientKey(r)
	h.adminAuthMu.Lock()
	defer h.adminAuthMu.Unlock()
	if h.adminAttempts == nil {
		h.adminAttempts = make(map[string]adminAuthAttempt)
	}
	h.cleanupAdminAttemptsLocked(now)
	attempt := h.adminAttempts[key]
	if now.Before(attempt.LockedUntil) {
		return false, time.Until(attempt.LockedUntil)
	}
	if attempt.WindowStarted.IsZero() || now.Sub(attempt.WindowStarted) >= adminAttemptWindow {
		delete(h.adminAttempts, key)
	}
	return true, 0
}

func (h *Handler) recordAdminAuthFailure(r *http.Request) time.Duration {
	now := time.Now()
	key := adminClientKey(r)
	h.adminAuthMu.Lock()
	defer h.adminAuthMu.Unlock()
	if h.adminAttempts == nil {
		h.adminAttempts = make(map[string]adminAuthAttempt)
	}
	h.cleanupAdminAttemptsLocked(now)
	attempt := h.adminAttempts[key]
	if attempt.WindowStarted.IsZero() || now.Sub(attempt.WindowStarted) >= adminAttemptWindow {
		attempt = adminAuthAttempt{WindowStarted: now}
	}
	attempt.Failures++
	if attempt.Failures >= adminMaxAttempts {
		attempt.LockedUntil = now.Add(adminAttemptLockDuration)
	}
	h.adminAttempts[key] = attempt
	if now.Before(attempt.LockedUntil) {
		return time.Until(attempt.LockedUntil)
	}
	return 0
}

func (h *Handler) cleanupAdminAttemptsLocked(now time.Time) {
	if len(h.adminAttempts) < adminMaxAttemptEntries {
		return
	}
	for key, attempt := range h.adminAttempts {
		windowExpired := attempt.WindowStarted.IsZero() || now.Sub(attempt.WindowStarted) >= adminAttemptWindow
		if windowExpired && !now.Before(attempt.LockedUntil) {
			delete(h.adminAttempts, key)
		}
	}
	for len(h.adminAttempts) >= adminMaxAttemptEntries {
		for key := range h.adminAttempts {
			delete(h.adminAttempts, key)
			break
		}
	}
}

func (h *Handler) clearAdminAuthFailures(r *http.Request) {
	h.adminAuthMu.Lock()
	delete(h.adminAttempts, adminClientKey(r))
	h.adminAuthMu.Unlock()
}

func adminSessionKey(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

func requestUsesHTTPS(r *http.Request) bool {
	if r != nil && r.TLS != nil {
		return true
	}
	return r != nil && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (h *Handler) issueAdminSession(w http.ResponseWriter, r *http.Request, duration time.Duration, persistent bool) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(duration)

	h.adminAuthMu.Lock()
	if h.adminSessions == nil {
		h.adminSessions = make(map[[32]byte]adminSession)
	}
	h.cleanupAdminSessionsLocked(time.Now())
	if len(h.adminSessions) >= adminMaxSessions {
		var oldestKey [32]byte
		var oldestExpiry time.Time
		for key, session := range h.adminSessions {
			if oldestExpiry.IsZero() || session.ExpiresAt.Before(oldestExpiry) {
				oldestKey = key
				oldestExpiry = session.ExpiresAt
			}
		}
		delete(h.adminSessions, oldestKey)
	}
	h.adminSessions[adminSessionKey(token)] = adminSession{ExpiresAt: expires}
	h.adminAuthMu.Unlock()

	cookie := &http.Cookie{
		Name:     adminSessionCookie,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   requestUsesHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	}
	if persistent {
		cookie.Expires = expires
		cookie.MaxAge = int(duration / time.Second)
	}
	http.SetCookie(w, cookie)
	// Remove the legacy cookie that stored the raw password.
	http.SetCookie(w, &http.Cookie{Name: "admin_password", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0), HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return nil
}

func (h *Handler) cleanupAdminSessionsLocked(now time.Time) {
	for key, session := range h.adminSessions {
		if !now.Before(session.ExpiresAt) {
			delete(h.adminSessions, key)
		}
	}
}

func (h *Handler) validateAdminSession(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	key := adminSessionKey(token)
	h.adminAuthMu.Lock()
	defer h.adminAuthMu.Unlock()
	if h.adminSessions == nil {
		return false
	}
	session, ok := h.adminSessions[key]
	if !ok || !now.Before(session.ExpiresAt) {
		delete(h.adminSessions, key)
		return false
	}
	return true
}

func (h *Handler) revokeAdminSession(r *http.Request) {
	if cookie, err := r.Cookie(adminSessionCookie); err == nil {
		h.adminAuthMu.Lock()
		delete(h.adminSessions, adminSessionKey(cookie.Value))
		h.adminAuthMu.Unlock()
	}
}

func expireAdminSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: adminSessionCookie, Path: "/admin", MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: true, Secure: requestUsesHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
}

func (h *Handler) clearAdminSessions() {
	h.adminAuthMu.Lock()
	h.adminSessions = make(map[[32]byte]adminSession)
	h.adminAuthMu.Unlock()
}

func (h *Handler) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	allowed, retryAfter := h.adminAttemptAllowed(r)
	if !allowed {
		w.Header().Set("Retry-After", retryAfterSeconds(retryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Too many login attempts"})
		return
	}
	var req struct {
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if !config.VerifyPassword(req.Password) {
		if retry := h.recordAdminAuthFailure(r); retry > 0 {
			w.Header().Set("Retry-After", retryAfterSeconds(retry))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Too many login attempts"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	h.clearAdminAuthFailures(r)
	duration := adminSessionDuration
	if req.Remember {
		duration = adminRememberDuration
	}
	if err := h.issueAdminSession(w, r, duration, req.Remember); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Failed to create session"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func retryAfterSeconds(duration time.Duration) string {
	seconds := int64((duration + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}

func (h *Handler) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	h.revokeAdminSession(r)
	expireAdminSessionCookie(w, r)
	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) verifyAdminReauthentication(w http.ResponseWriter, r *http.Request, password string) bool {
	if allowed, retry := h.adminAttemptAllowed(r); !allowed {
		w.Header().Set("Retry-After", retryAfterSeconds(retry))
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Too many authentication attempts"})
		return false
	}
	if !config.VerifyPassword(password) {
		if retry := h.recordAdminAuthFailure(r); retry > 0 {
			w.Header().Set("Retry-After", retryAfterSeconds(retry))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Too many authentication attempts"})
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Re-authentication failed"})
		return false
	}
	h.clearAdminAuthFailures(r)
	return true
}

func (h *Handler) authenticateAdminRequest(r *http.Request) (authenticated bool, sessionAuth bool, throttledFor time.Duration) {
	if cookie, err := r.Cookie(adminSessionCookie); err == nil && h.validateAdminSession(cookie.Value) {
		return true, true, 0
	}
	password := r.Header.Get("X-Admin-Password")
	if password == "" {
		return false, false, 0
	}
	if allowed, retry := h.adminAttemptAllowed(r); !allowed {
		return false, false, retry
	}
	if config.VerifyPassword(password) {
		h.clearAdminAuthFailures(r)
		return true, false, 0
	}
	return false, false, h.recordAdminAuthFailure(r)
}

func adminRequestOriginAllowed(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site == "cross-site" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}
