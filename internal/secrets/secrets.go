package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const keySize = 32

// Cipher 使用派生后的独立密钥分别进行加密和去重摘要计算。
type Cipher struct {
	encryptionKey []byte
	hashKey       []byte
}

// GenerateKey 生成可用于 security.master_key 的随机主密钥。
func GenerateKey() (string, error) {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("生成随机密钥失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// ParseKey 解析 32 字节主密钥的 Base64 或十六进制表示。
func ParseKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, fmt.Errorf("未设置 security.master_key")
	}

	decoders := []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, decode := range decoders {
		key, err := decode(encoded)
		if err == nil && len(key) == keySize {
			return key, nil
		}
	}
	return nil, fmt.Errorf("security.master_key 必须是 32 字节密钥的 Base64 或十六进制表示")
}

// NewCipher 派生加密密钥和去重摘要密钥，避免跨用途复用同一密钥。
func NewCipher(masterKey []byte) (*Cipher, error) {
	if len(masterKey) != keySize {
		return nil, fmt.Errorf("主密钥长度必须为 %d 字节", keySize)
	}
	return &Cipher{
		encryptionKey: derive(masterKey, "kimi-monitor/encryption/v1"),
		hashKey:       derive(masterKey, "kimi-monitor/dedup/v1"),
	}, nil
}

func derive(masterKey []byte, label string) []byte {
	mac := hmac.New(sha256.New, masterKey)
	_, _ = mac.Write([]byte(label))
	return mac.Sum(nil)
}

// Encrypt 使用 AES-256-GCM 加密，返回值由随机 nonce 和密文组成。
func (c *Cipher) Encrypt(plaintext string) ([]byte, error) {
	return c.seal(plaintext, "kimi-token-v1")
}

// EncryptWebhook 将机器人密钥与 API Key 密文的用途隔离。
func (c *Cipher) EncryptWebhook(webhookURL string) ([]byte, error) {
	return c.seal(webhookURL, "wecom-webhook-v1")
}

func (c *Cipher) seal(plaintext, purpose string) ([]byte, error) {
	block, err := aes.NewCipher(c.encryptionKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成加密 nonce 失败: %w", err)
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), []byte(purpose)), nil
}

// Decrypt 解密并认证数据库中的令牌密文。
func (c *Cipher) Decrypt(ciphertext []byte) (string, error) {
	return c.open(ciphertext, "kimi-token-v1")
}

func (c *Cipher) DecryptWebhook(ciphertext []byte) (string, error) {
	return c.open(ciphertext, "wecom-webhook-v1")
}

func (c *Cipher) open(ciphertext []byte, purpose string) (string, error) {
	block, err := aes.NewCipher(c.encryptionKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return "", fmt.Errorf("密文长度无效")
	}
	nonce := ciphertext[:gcm.NonceSize()]
	plaintext, err := gcm.Open(nil, nonce, ciphertext[gcm.NonceSize():], []byte(purpose))
	if err != nil {
		return "", fmt.Errorf("解密失败，请检查 security.master_key: %w", err)
	}
	return string(plaintext), nil
}

// Digest 计算带密钥的 SHA-256 摘要，用于唯一约束去重。
func (c *Cipher) Digest(plaintext string) []byte {
	mac := hmac.New(sha256.New, c.hashKey)
	_, _ = mac.Write([]byte(plaintext))
	return mac.Sum(nil)
}

// MaskToken 生成 sk-abcd****0123 形式的管理名称。
func MaskToken(token string) (string, error) {
	if strings.HasPrefix(token, "sk-admin-") {
		return "", fmt.Errorf("额度监控需要推理 API Key，请使用普通 Kimi 令牌")
	}
	if !strings.HasPrefix(token, "sk-") {
		return "", fmt.Errorf("kimi 令牌必须以 sk- 开头")
	}
	if len(token) < len("sk-")+12 {
		return "", fmt.Errorf("kimi 令牌长度不足，无法安全打码")
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return "", fmt.Errorf("kimi 令牌包含空白或非 ASCII 字符")
		}
	}
	return token[:len("sk-")+4] + "****" + token[len(token)-4:], nil
}
