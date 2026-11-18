package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func TestEncryptDecryptAndDigest(t *testing.T) {
	t.Parallel()
	cipher := testCipher(t)
	plaintext := "sk-abcdefghijklmnop12345678"
	first, err := cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("AES-GCM 使用随机 nonce，两次密文不应相同")
	}
	if bytes.Contains(first, []byte(plaintext)) {
		t.Fatal("密文不应包含令牌明文")
	}
	decrypted, err := cipher.Decrypt(first)
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != plaintext {
		t.Fatalf("解密结果 = %q，期望 %q", decrypted, plaintext)
	}
	first[len(first)-1] ^= 1
	if _, err := cipher.Decrypt(first); err == nil {
		t.Fatal("被篡改的密文必须认证失败")
	}
	if !bytes.Equal(cipher.Digest(plaintext), cipher.Digest(plaintext)) {
		t.Fatal("同一令牌的 HMAC 摘要必须稳定")
	}
}

func TestMaskToken(t *testing.T) {
	t.Parallel()
	name, err := MaskToken("sk-abcdefghijklmnop12345678")
	if err != nil {
		t.Fatal(err)
	}
	if name != "sk-abcd****5678" {
		t.Fatalf("打码名称 = %q", name)
	}
	for _, invalid := range []string{"", "abc", "sk-short", "sk-abcd efgh12345678"} {
		if _, err := MaskToken(invalid); err == nil {
			t.Errorf("无效令牌 %q 应被拒绝", invalid)
		}
	}
}

func TestParseKey(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{7}, 32)
	for _, encoded := range []string{
		base64.StdEncoding.EncodeToString(key),
		base64.RawStdEncoding.EncodeToString(key),
		hex.EncodeToString(key),
	} {
		parsed, err := ParseKey(encoded)
		if err != nil {
			t.Fatalf("ParseKey 返回错误: %v", err)
		}
		if !bytes.Equal(parsed, key) {
			t.Fatal("解析后的密钥不一致")
		}
	}
	if _, err := ParseKey(strings.Repeat("a", 10)); err == nil {
		t.Fatal("错误长度的密钥应被拒绝")
	}
}
