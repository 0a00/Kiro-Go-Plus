package proxy

import (
	"encoding/json"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"sync"
	"time"
)

const kiroSsoChoiceTTL = 5 * time.Minute

type pendingKiroSsoChoice struct {
	result    *auth.KiroSsoResult
	machineID string
	profiles  []KiroProfile
	expiresAt int64
	deadline  time.Time
	timer     *time.Timer
}

var (
	pendingKiroSsoChoices   = make(map[string]*pendingKiroSsoChoice)
	pendingKiroSsoChoicesMu sync.Mutex
)

func (h *Handler) apiStartKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	session, signInURL, err := auth.StartKiroSsoLogin(req.Region)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": session.ID,
		"signInUrl": signInURL,
		"interval":  2,
	})
}

func (h *Handler) apiCancelKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.SessionID != "" {
		auth.CancelKiroSsoLogin(req.SessionID)
		dropPendingKiroSsoChoice(req.SessionID)
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiPollKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.SessionID) == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "sessionId is required"})
		return
	}
	result, status, err := auth.PollKiroSsoAuthContext(r.Context(), req.SessionID)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if status == "pending" {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "completed": false, "status": "pending",
		})
		return
	}

	machineID := config.GenerateMachineId()
	expiresIn := result.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiresAt := time.Now().Unix() + int64(expiresIn)
	if strings.EqualFold(result.AuthMethod, "external_idp") && strings.TrimSpace(result.ProfileArn) == "" {
		probe := &config.Account{
			Email: result.Email, AccessToken: result.AccessToken,
			AuthMethod: result.AuthMethod, Provider: result.Provider,
			Region: result.Region, MachineId: machineID,
		}
		profiles, discoveryErr := DiscoverKiroProfilesContext(r.Context(), probe)
		if discoveryErr != nil {
			logger.Warnf("[KiroSSO] Profile discovery failed for %s: %v", accountEmailForLog(probe), discoveryErr)
		}
		switch {
		case len(profiles) == 1:
			result.ProfileArn = profiles[0].Arn
		case len(profiles) >= 2:
			stashPendingKiroSsoChoice(req.SessionID, &pendingKiroSsoChoice{
				result: result, machineID: machineID, profiles: profiles,
				expiresAt: expiresAt, deadline: time.Now().Add(kiroSsoChoiceTTL),
			})
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "completed": true, "status": "choose_profile", "profiles": profiles,
			})
			return
		}
	}
	h.finalizeKiroSsoAccount(w, result, machineID, expiresAt)
}

func (h *Handler) apiSelectKiroSsoProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID  string `json:"sessionId"`
		ProfileArn string `json:"profileArn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.ProfileArn = strings.TrimSpace(req.ProfileArn)
	if req.SessionID == "" || req.ProfileArn == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "sessionId and profileArn are required"})
		return
	}
	pending := takePendingKiroSsoChoice(req.SessionID)
	if pending == nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "profile choice expired; sign in again"})
		return
	}
	valid := false
	for _, profile := range pending.profiles {
		if profile.Arn == req.ProfileArn {
			valid = true
			break
		}
	}
	if !valid {
		stashPendingKiroSsoChoice(req.SessionID, pending)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is not one of the discovered profiles"})
		return
	}
	pending.result.ProfileArn = req.ProfileArn
	h.finalizeKiroSsoAccount(w, pending.result, pending.machineID, pending.expiresAt)
}

func buildKiroSsoAccount(result *auth.KiroSsoResult, machineID string, expiresAt int64) config.Account {
	return config.Account{
		ID: auth.GenerateAccountID(), Email: result.Email,
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken,
		ClientID: result.ClientID, AuthMethod: result.AuthMethod,
		Provider: result.Provider, Region: result.Region,
		ProfileArn: result.ProfileArn, TokenEndpoint: result.TokenEndpoint,
		IssuerURL: result.IssuerURL, Scopes: result.Scopes,
		ExpiresAt: expiresAt, Enabled: true, MachineId: machineID,
	}
}

func (h *Handler) finalizeKiroSsoAccount(w http.ResponseWriter, result *auth.KiroSsoResult, machineID string, expiresAt int64) {
	account := buildKiroSsoAccount(result, machineID, expiresAt)
	persisted, updated, err := config.UpsertAccountByIdentity(account)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	if persisted.Enabled && persisted.AccessToken != "" && persisted.ProfileArn != "" {
		h.refreshModelCachesAsync([]config.Account{persisted})
	}
	action := "created"
	if updated {
		action = "updated"
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "completed": true, "action": action,
		"account": map[string]interface{}{
			"id": persisted.ID, "email": persisted.Email, "authMethod": persisted.AuthMethod,
		},
	})
}

func (h *Handler) apiListAccountKiroProfiles(w http.ResponseWriter, r *http.Request, id string) {
	account, status, errMessage := h.lookupAccountForKiroProfiles(id)
	if account == nil {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": errMessage})
		return
	}
	if err := h.ensureValidTokenContext(r.Context(), account); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "token refresh failed: " + err.Error()})
		return
	}
	profiles, err := DiscoverKiroProfilesContext(r.Context(), account)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "profiles": profiles, "current": strings.TrimSpace(account.ProfileArn),
	})
}

func (h *Handler) apiSwitchAccountKiroProfile(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		ProfileArn string `json:"profileArn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ProfileArn = strings.TrimSpace(req.ProfileArn)
	if req.ProfileArn == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is required"})
		return
	}
	account, status, errMessage := h.lookupAccountForKiroProfiles(id)
	if account == nil {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": errMessage})
		return
	}
	if err := h.ensureValidTokenContext(r.Context(), account); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "token refresh failed: " + err.Error()})
		return
	}
	profiles, err := DiscoverKiroProfilesContext(r.Context(), account)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	valid := false
	for _, profile := range profiles {
		if profile.Arn == req.ProfileArn {
			valid = true
			break
		}
	}
	if !valid {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is not one of the discovered profiles"})
		return
	}
	if err := config.UpdateAccountProfileArn(id, req.ProfileArn); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.UpdateProfileArn(id, req.ProfileArn)
	account.ProfileArn = req.ProfileArn
	modelsRefreshed := true
	if err := h.fetchAndCacheAccountModelsContext(r.Context(), account); err != nil {
		modelsRefreshed = false
		logger.Warnf("[ProfileArn] Model refresh after profile switch failed for %s: %v", accountEmailForLog(account), err)
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "profileArn": req.ProfileArn, "modelsRefreshed": modelsRefreshed,
	})
}

func (h *Handler) lookupAccountForKiroProfiles(id string) (*config.Account, int, string) {
	for _, stored := range config.GetAccounts() {
		if stored.ID != id {
			continue
		}
		account := stored
		if !strings.EqualFold(strings.TrimSpace(account.AuthMethod), "external_idp") {
			return nil, http.StatusBadRequest, "profile switching is only supported for external_idp accounts"
		}
		if latest := h.pool.GetByID(id); latest != nil {
			account.AccessToken = latest.AccessToken
			account.RefreshToken = latest.RefreshToken
			account.ExpiresAt = latest.ExpiresAt
			account.ProfileArn = latest.ProfileArn
		}
		return &account, http.StatusOK, ""
	}
	return nil, http.StatusNotFound, "Account not found"
}

func stashPendingKiroSsoChoice(sessionID string, pending *pendingKiroSsoChoice) {
	if pending == nil {
		return
	}
	delay := time.Until(pending.deadline)
	if delay < 0 {
		delay = 0
	}
	pending.timer = time.AfterFunc(delay, func() {
		pendingKiroSsoChoicesMu.Lock()
		if current := pendingKiroSsoChoices[sessionID]; current == pending {
			delete(pendingKiroSsoChoices, sessionID)
		}
		pendingKiroSsoChoicesMu.Unlock()
	})
	pendingKiroSsoChoicesMu.Lock()
	if previous := pendingKiroSsoChoices[sessionID]; previous != nil && previous.timer != nil {
		previous.timer.Stop()
	}
	pendingKiroSsoChoices[sessionID] = pending
	pendingKiroSsoChoicesMu.Unlock()
}

func takePendingKiroSsoChoice(sessionID string) *pendingKiroSsoChoice {
	pendingKiroSsoChoicesMu.Lock()
	defer pendingKiroSsoChoicesMu.Unlock()
	pending := pendingKiroSsoChoices[sessionID]
	delete(pendingKiroSsoChoices, sessionID)
	if pending != nil && pending.timer != nil {
		pending.timer.Stop()
	}
	return pending
}

func dropPendingKiroSsoChoice(sessionID string) {
	_ = takePendingKiroSsoChoice(sessionID)
}
