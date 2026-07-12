package proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var responsesStoreMu sync.Mutex

var responsesIndex = struct {
	dir        string
	loaded     bool
	files      map[string]responseFileInfo
	totalBytes int64
}{files: make(map[string]responseFileInfo)}

const (
	responsesDirName           = "responses"
	responsesDefaultTTL        = 30 * 24 * time.Hour
	responsesEncryptionVersion = 1
	responsesEncryptionPurpose = "responses-storage-v1"
)

type encryptedResponseEnvelope struct {
	EncryptionVersion int    `json:"encryption_version"`
	Algorithm         string `json:"algorithm"`
	Nonce             string `json:"nonce"`
	Ciphertext        string `json:"ciphertext"`
}

func responsesDir() string {
	return filepath.Join(config.GetConfigDir(), responsesDirName)
}

func generateResponseID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("resp_%d%012x", time.Now().UnixNano(), 0)
	}
	return "resp_" + hex.EncodeToString(buf) + fmt.Sprintf("%08x", time.Now().Unix()&0xffffffff)
}

func generateOutputItemID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

func saveResponse(resp *ResponsesObject) error {
	if resp == nil || resp.ID == "" {
		return fmt.Errorf("response missing id")
	}
	key, err := config.DeriveEncryptionKey(responsesEncryptionPurpose)
	if err != nil {
		return fmt.Errorf("Responses storage encryption: %w", err)
	}
	responsesStoreMu.Lock()
	defer responsesStoreMu.Unlock()

	dir := responsesDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create responses dir: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	ensureResponsesIndexLocked(dir)
	if resp.StoredAt == 0 {
		resp.StoredAt = time.Now().Unix()
	}

	persisted := storedResponseDoc{
		ID:                 resp.ID,
		Object:             resp.Object,
		CreatedAt:          resp.CreatedAt,
		Status:             resp.Status,
		Model:              resp.Model,
		Output:             resp.Output,
		Usage:              resp.Usage,
		PreviousResponseID: resp.PreviousResponseID,
		Metadata:           resp.Metadata,
		IncompleteDetails:  resp.IncompleteDetails,
		Instructions:       resp.Instructions,
		StoredInput:        resp.StoredInput,
		StoredAt:           resp.StoredAt,
		OwnerAPIKeyID:      resp.OwnerAPIKeyID,
	}

	path := filepath.Join(dir, sanitizeResponseID(resp.ID)+".json")
	plain, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("marshal stored response: %w", err)
	}
	data, err := encryptStoredResponse(key, resp.ID, plain)
	if err != nil {
		return fmt.Errorf("encrypt stored response: %w", err)
	}
	if err := commitResponseFileLocked(path, data); err != nil {
		return err
	}
	enforceResponsesQuotaLocked(config.GetResponsesStorageConfig())
	return nil
}

func loadResponse(id string) (*ResponsesObject, error) {
	return loadResponseDocument(id, "", false)
}

func loadResponseForOwner(id, ownerAPIKeyID string) (*ResponsesObject, error) {
	return loadResponseDocument(id, ownerAPIKeyID, true)
}

func loadResponseDocument(id, ownerAPIKeyID string, enforceOwner bool) (*ResponsesObject, error) {
	if id == "" {
		return nil, fmt.Errorf("empty response id")
	}
	key, err := config.DeriveEncryptionKey(responsesEncryptionPurpose)
	if err != nil {
		return nil, fmt.Errorf("Responses storage encryption: %w", err)
	}
	responsesStoreMu.Lock()
	defer responsesStoreMu.Unlock()
	dir := responsesDir()
	ensureResponsesIndexLocked(dir)
	path := filepath.Join(dir, sanitizeResponseID(id)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc, legacyPlaintext, err := decodeStoredResponse(key, id, data)
	if err != nil {
		return nil, err
	}
	if doc.ID != id {
		return nil, fmt.Errorf("stored response id does not match requested id")
	}
	if legacyPlaintext {
		plain, marshalErr := json.Marshal(doc)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal legacy stored response: %w", marshalErr)
		}
		encrypted, encryptErr := encryptStoredResponse(key, id, plain)
		if encryptErr != nil {
			return nil, fmt.Errorf("encrypt legacy stored response: %w", encryptErr)
		}
		if writeErr := commitResponseFileLocked(path, encrypted); writeErr != nil {
			return nil, fmt.Errorf("migrate legacy stored response: %w", writeErr)
		}
	}
	ttl := time.Duration(config.GetResponsesStorageConfig().TTLHours) * time.Hour
	if ttl <= 0 {
		ttl = responsesDefaultTTL
	}
	if doc.StoredAt > 0 && time.Since(time.Unix(doc.StoredAt, 0)) > ttl {
		removeResponseFileLocked(filepath.Base(path))
		return nil, fmt.Errorf("stored response expired")
	}
	if enforceOwner && doc.OwnerAPIKeyID != ownerAPIKeyID {
		return nil, fmt.Errorf("stored response belongs to a different API key")
	}
	return &ResponsesObject{
		ID:                 doc.ID,
		Object:             doc.Object,
		CreatedAt:          doc.CreatedAt,
		Status:             doc.Status,
		Model:              doc.Model,
		Output:             doc.Output,
		Usage:              doc.Usage,
		PreviousResponseID: doc.PreviousResponseID,
		Metadata:           doc.Metadata,
		IncompleteDetails:  doc.IncompleteDetails,
		Instructions:       doc.Instructions,
		StoredInput:        doc.StoredInput,
		StoredAt:           doc.StoredAt,
		OwnerAPIKeyID:      doc.OwnerAPIKeyID,
	}, nil
}

func purgeExpiredResponses(ttl time.Duration) {
	if ttl <= 0 {
		ttl = responsesDefaultTTL
	}
	responsesStoreMu.Lock()
	defer responsesStoreMu.Unlock()
	dir := responsesDir()
	ensureResponsesIndexLocked(dir)
	cutoff := time.Now().Add(-ttl)
	for name, file := range responsesIndex.files {
		if file.modTime.Before(cutoff) {
			if err := removeResponseFileLocked(name); err != nil {
				logger.Warnf("[Responses] purge %s failed: %v", name, err)
			}
		}
	}
	enforceResponsesQuotaLocked(config.GetResponsesStorageConfig())
}

func purgeResponsesStorage() {
	settings := config.GetResponsesStorageConfig()
	purgeExpiredResponses(time.Duration(settings.TTLHours) * time.Hour)
}

func purgeAllResponsesStorage() (int, int64, error) {
	responsesStoreMu.Lock()
	defer responsesStoreMu.Unlock()
	dir := responsesDir()
	ensureResponsesIndexLocked(dir)
	count := len(responsesIndex.files)
	bytes := responsesIndex.totalBytes
	if err := os.RemoveAll(dir); err != nil {
		return 0, 0, err
	}
	responsesIndex.dir = dir
	responsesIndex.loaded = true
	responsesIndex.files = make(map[string]responseFileInfo)
	responsesIndex.totalBytes = 0
	return count, bytes, nil
}

func responsesStorageStats() (int, int64) {
	responsesStoreMu.Lock()
	defer responsesStoreMu.Unlock()
	ensureResponsesIndexLocked(responsesDir())
	return len(responsesIndex.files), responsesIndex.totalBytes
}

func responsesStorageEncryptionEnabled() bool {
	_, err := config.DeriveEncryptionKey(responsesEncryptionPurpose)
	return err == nil
}

type responseFileInfo struct {
	path    string
	name    string
	size    int64
	modTime time.Time
}

func enforceResponsesQuotaLocked(settings config.ResponsesStorageConfig) {
	dir := responsesDir()
	ensureResponsesIndexLocked(dir)
	files := make([]responseFileInfo, 0, len(responsesIndex.files))
	for _, file := range responsesIndex.files {
		files = append(files, file)
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].name < files[j].name
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	maxFiles := settings.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 10000
	}
	maxBytes := settings.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 30
	}
	for len(files) > 0 && (len(files) > maxFiles || responsesIndex.totalBytes > maxBytes) {
		oldest := files[0]
		files = files[1:]
		if err := removeResponseFileLocked(oldest.name); err != nil {
			logger.Warnf("[Responses] quota purge %s failed: %v", oldest.name, err)
			continue
		}
	}
}

func ensureResponsesIndexLocked(dir string) {
	if responsesIndex.loaded && responsesIndex.dir == dir {
		return
	}
	responsesIndex.dir = dir
	responsesIndex.loaded = true
	responsesIndex.files = make(map[string]responseFileInfo)
	responsesIndex.totalBytes = 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		file := responseFileInfo{
			path:    filepath.Join(dir, entry.Name()),
			name:    entry.Name(),
			size:    info.Size(),
			modTime: info.ModTime(),
		}
		responsesIndex.files[file.name] = file
		responsesIndex.totalBytes += file.size
	}
}

func removeResponseFileLocked(name string) error {
	file, ok := responsesIndex.files[name]
	if !ok {
		return os.Remove(filepath.Join(responsesDir(), name))
	}
	err := os.Remove(file.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(responsesIndex.files, name)
	responsesIndex.totalBytes -= file.size
	if responsesIndex.totalBytes < 0 {
		responsesIndex.totalBytes = 0
	}
	return nil
}

func commitResponseFileLocked(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write stored response: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit stored response: %w", err)
	}
	name := filepath.Base(path)
	if previous, ok := responsesIndex.files[name]; ok {
		responsesIndex.totalBytes -= previous.size
	}
	file := responseFileInfo{path: path, name: name, size: int64(len(data)), modTime: time.Now()}
	if info, statErr := os.Stat(path); statErr == nil {
		file.size = info.Size()
		file.modTime = info.ModTime()
	}
	responsesIndex.files[name] = file
	responsesIndex.totalBytes += file.size
	return nil
}

func encryptStoredResponse(key []byte, id string, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	aad := []byte("kiro-go-response:" + sanitizeResponseID(id))
	ciphertext := gcm.Seal(nil, nonce, plain, aad)
	envelope := encryptedResponseEnvelope{
		EncryptionVersion: responsesEncryptionVersion,
		Algorithm:         "AES-256-GCM",
		Nonce:             base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext:        base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	return json.MarshalIndent(envelope, "", "  ")
}

func decodeStoredResponse(key []byte, id string, data []byte) (storedResponseDoc, bool, error) {
	var envelope encryptedResponseEnvelope
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.EncryptionVersion != 0 {
		if envelope.EncryptionVersion != responsesEncryptionVersion || envelope.Algorithm != "AES-256-GCM" {
			return storedResponseDoc{}, false, fmt.Errorf("unsupported stored response encryption version")
		}
		nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
		if err != nil {
			return storedResponseDoc{}, false, fmt.Errorf("decode stored response nonce: %w", err)
		}
		ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
		if err != nil {
			return storedResponseDoc{}, false, fmt.Errorf("decode stored response ciphertext: %w", err)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return storedResponseDoc{}, false, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return storedResponseDoc{}, false, err
		}
		if len(nonce) != gcm.NonceSize() {
			return storedResponseDoc{}, false, fmt.Errorf("stored response nonce has invalid length")
		}
		aad := []byte("kiro-go-response:" + sanitizeResponseID(id))
		plain, err := gcm.Open(nil, nonce, ciphertext, aad)
		if err != nil {
			return storedResponseDoc{}, false, fmt.Errorf("decrypt stored response: %w", err)
		}
		var doc storedResponseDoc
		if err := json.Unmarshal(plain, &doc); err != nil {
			return storedResponseDoc{}, false, fmt.Errorf("decode stored response: %w", err)
		}
		return doc, false, nil
	}

	var doc storedResponseDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return storedResponseDoc{}, false, fmt.Errorf("decode stored response: %w", err)
	}
	return doc, true, nil
}

func logResponsesPersistFailure(id string, err error) {
	logger.Warnf("[Responses] persist %s failed: %v", id, err)
}

func sanitizeResponseID(id string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '_' || r == '-':
			return r
		default:
			return -1
		}
	}, id)
	if cleaned == "" {
		return "invalid"
	}
	return cleaned
}

type storedResponseDoc struct {
	ID                 string               `json:"id"`
	Object             string               `json:"object"`
	CreatedAt          int64                `json:"created_at"`
	Status             string               `json:"status"`
	Model              string               `json:"model"`
	Output             []ResponseOutputItem `json:"output"`
	Usage              ResponsesUsage       `json:"usage"`
	PreviousResponseID string               `json:"previous_response_id,omitempty"`
	Metadata           map[string]string    `json:"metadata,omitempty"`
	IncompleteDetails  *IncompleteDetails   `json:"incomplete_details,omitempty"`
	Instructions       string               `json:"instructions,omitempty"`
	StoredInput        json.RawMessage      `json:"stored_input,omitempty"`
	StoredAt           int64                `json:"stored_at"`
	OwnerAPIKeyID      string               `json:"owner_api_key_id,omitempty"`
}
