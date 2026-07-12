package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const encryptedSecretPrefix = "enc:v1:"

type configSecretField struct {
	name  string
	value *string
}

func masterKeyFromEnvironment() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv("KIRO_MASTER_KEY"))
	if path := strings.TrimSpace(os.Getenv("KIRO_MASTER_KEY_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read KIRO_MASTER_KEY_FILE: %w", err)
		}
		raw = strings.TrimSpace(string(data))
	}
	if raw == "" {
		return nil, nil
	}

	candidates := make([][]byte, 0, 3)
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		candidates = append(candidates, decoded)
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		candidates = append(candidates, decoded)
	}
	if decoded, err := hex.DecodeString(raw); err == nil {
		candidates = append(candidates, decoded)
	}
	candidates = append(candidates, []byte(raw))
	for _, candidate := range candidates {
		if len(candidate) == 32 {
			return candidate, nil
		}
	}
	return nil, errors.New("KIRO_MASTER_KEY must decode to exactly 32 bytes")
}

func configSecretFields(value *Config) []configSecretField {
	if value == nil {
		return nil
	}
	fields := []configSecretField{
		{name: "apiKey", value: &value.ApiKey},
		{name: "proxyURL", value: &value.ProxyURL},
		{name: "health.webhookURL", value: &value.Health.WebhookURL},
		{name: "countTokens.apiKey", value: &value.CountTokensProvider.ApiKey},
		{name: "countTokens.legacyApiKey", value: &value.CountTokensApiKey},
	}
	for i := range value.ApiKeys {
		fields = append(fields, configSecretField{name: "apiKeys.key", value: &value.ApiKeys[i].Key})
	}
	for i := range value.Accounts {
		account := &value.Accounts[i]
		fields = append(fields,
			configSecretField{name: "account.accessToken", value: &account.AccessToken},
			configSecretField{name: "account.refreshToken", value: &account.RefreshToken},
			configSecretField{name: "account.kiroApiKey", value: &account.KiroApiKey},
			configSecretField{name: "account.clientSecret", value: &account.ClientSecret},
			configSecretField{name: "account.proxyURL", value: &account.ProxyURL},
		)
	}
	return fields
}

func prepareLoadedConfigSecrets(value *Config) (bool, error) {
	key, err := masterKeyFromEnvironment()
	if err != nil {
		return false, err
	}
	needsMigration := false
	for _, field := range configSecretFields(value) {
		secret := strings.TrimSpace(*field.value)
		if secret == "" {
			continue
		}
		if !strings.HasPrefix(secret, encryptedSecretPrefix) {
			if len(key) > 0 {
				needsMigration = true
			}
			continue
		}
		if len(key) == 0 {
			return false, fmt.Errorf("encrypted configuration requires KIRO_MASTER_KEY or KIRO_MASTER_KEY_FILE")
		}
		plain, err := decryptConfigSecret(key, field.name, secret)
		if err != nil {
			return false, fmt.Errorf("decrypt %s: %w", field.name, err)
		}
		*field.value = plain
	}
	return needsMigration, nil
}

func configForPersistence(value *Config) (*Config, error) {
	if value == nil {
		return nil, errors.New("config is not initialized")
	}
	snapshot := *value
	snapshot.Accounts = append([]Account(nil), value.Accounts...)
	snapshot.ApiKeys = append([]ApiKeyEntry(nil), value.ApiKeys...)

	key, err := masterKeyFromEnvironment()
	if err != nil {
		return nil, err
	}
	if len(key) == 0 {
		return &snapshot, nil
	}
	for _, field := range configSecretFields(&snapshot) {
		if *field.value == "" {
			continue
		}
		encrypted, err := encryptConfigSecret(key, field.name, *field.value)
		if err != nil {
			return nil, fmt.Errorf("encrypt %s: %w", field.name, err)
		}
		*field.value = encrypted
	}
	return &snapshot, nil
}

func encryptConfigSecret(key []byte, fieldName, plain string) (string, error) {
	gcm, err := newConfigGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plain), []byte(fieldName))
	payload := append(nonce, sealed...)
	return encryptedSecretPrefix + base64.RawStdEncoding.EncodeToString(payload), nil
}

func decryptConfigSecret(key []byte, fieldName, encrypted string) (string, error) {
	gcm, err := newConfigGCM(key)
	if err != nil {
		return "", err
	}
	payload, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(encrypted, encryptedSecretPrefix))
	if err != nil {
		return "", err
	}
	if len(payload) < gcm.NonceSize() {
		return "", errors.New("encrypted value is truncated")
	}
	nonce, ciphertext := payload[:gcm.NonceSize()], payload[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(fieldName))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func newConfigGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func CredentialEncryptionEnabled() bool {
	key, err := masterKeyFromEnvironment()
	return err == nil && len(key) == 32
}

// DeriveEncryptionKey returns a purpose-scoped key without exposing the
// configured master key directly to callers.
func DeriveEncryptionKey(purpose string) ([]byte, error) {
	purpose = strings.TrimSpace(purpose)
	if purpose == "" {
		return nil, errors.New("encryption key purpose is required")
	}
	key, err := masterKeyFromEnvironment()
	if err != nil {
		return nil, err
	}
	if len(key) == 0 {
		return nil, errors.New("KIRO_MASTER_KEY or KIRO_MASTER_KEY_FILE is required")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("kiro-go:" + purpose))
	return mac.Sum(nil), nil
}
