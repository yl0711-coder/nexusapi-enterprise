package crypto

import (
	"bytes"
	"strings"
	"testing"
)

func testKey(b byte) []byte {
	k := make([]byte, KeySize)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	kr, err := NewKeyring("v1", map[string][]byte{"v1": testKey(0x11)})
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	for _, pt := range []string{"", "sk-nexus-secret", "成员密码-中文-😀", strings.Repeat("x", 1000)} {
		ct, err := kr.EncryptString(pt)
		if err != nil {
			t.Fatalf("encrypt %q: %v", pt, err)
		}
		if !strings.HasPrefix(ct, "v1:v1:") {
			t.Errorf("ciphertext format unexpected: %q", ct)
		}
		if strings.Contains(ct, pt) && pt != "" {
			t.Errorf("ciphertext leaks plaintext: %q", ct)
		}
		got, err := kr.DecryptString(ct)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if got != pt {
			t.Errorf("round-trip mismatch: got %q want %q", got, pt)
		}
	}
}

// 同一明文两次加密必产生不同密文(随机 nonce),否则 GCM 复用致命。
func TestEncrypt_NonceIsRandom(t *testing.T) {
	kr, _ := NewKeyring("v1", map[string][]byte{"v1": testKey(0x22)})
	a, _ := kr.EncryptString("same-plaintext")
	b, _ := kr.EncryptString("same-plaintext")
	if a == b {
		t.Fatal("两次加密密文相同 —— nonce 被复用,GCM 安全性失效")
	}
}

// 篡改密文任意一段都应被 GCM 完整性校验挡下。
func TestDecrypt_TamperRejected(t *testing.T) {
	kr, _ := NewKeyring("v1", map[string][]byte{"v1": testKey(0x33)})
	ct, _ := kr.EncryptString("integrity-protected")
	parts := strings.Split(ct, ":")
	// 翻转密文体最后一个字符。
	body := []byte(parts[3])
	body[len(body)-1] ^= 0x01
	parts[3] = string(body)
	if _, err := kr.DecryptString(strings.Join(parts, ":")); err != ErrBadCiphertext {
		t.Errorf("tampered ciphertext should fail with ErrBadCiphertext, got %v", err)
	}
}

// 轮换:用 v2 当前加密,旧 v1 密文仍能解(按 key_id 选钥)。
func TestKeyring_RotationCoexist(t *testing.T) {
	old, _ := NewKeyring("v1", map[string][]byte{"v1": testKey(0x44)})
	ctV1, _ := old.EncryptString("rotate-me")

	rotated, err := NewKeyring("v2", map[string][]byte{
		"v1": testKey(0x44),
		"v2": testKey(0x55),
	})
	if err != nil {
		t.Fatalf("rotated keyring: %v", err)
	}
	if rotated.CurrentKeyID() != "v2" {
		t.Errorf("current key should be v2, got %s", rotated.CurrentKeyID())
	}
	// 旧密文(v1)仍可解。
	got, err := rotated.DecryptString(ctV1)
	if err != nil || got != "rotate-me" {
		t.Errorf("old v1 ciphertext should decrypt under rotated keyring: got %q err %v", got, err)
	}
	// 新加密用 v2。
	ctV2, _ := rotated.EncryptString("new-data")
	if id, _ := KeyIDOf(ctV2); id != "v2" {
		t.Errorf("new ciphertext should use v2, got %s", id)
	}
}

func TestDecrypt_UnknownKeyID(t *testing.T) {
	kr, _ := NewKeyring("v1", map[string][]byte{"v1": testKey(0x66)})
	ct, _ := kr.EncryptString("x")
	// 用一个不含 v1 的钥匙环解 → 未知 key_id。
	other, _ := NewKeyring("v9", map[string][]byte{"v9": testKey(0x77)})
	if _, err := other.DecryptString(ct); err != ErrUnknownKeyID {
		t.Errorf("want ErrUnknownKeyID, got %v", err)
	}
}

func TestNewKeyring_Validation(t *testing.T) {
	if _, err := NewKeyring("v1", nil); err != ErrEmptyKeyring {
		t.Errorf("empty keyring should error")
	}
	if _, err := NewKeyring("v1", map[string][]byte{"v1": testKey(0x11)[:16]}); err == nil {
		t.Errorf("16-byte key should be rejected (need 32)")
	}
	if _, err := NewKeyring("missing", map[string][]byte{"v1": testKey(0x11)}); err == nil {
		t.Errorf("current id not in set should error")
	}
}

func TestNewKeyringFromBase64(t *testing.T) {
	// 32 字节全 0 的 base64。
	kr, err := NewKeyringFromBase64("", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatalf("from base64: %v", err)
	}
	ct, _ := kr.EncryptString("hi")
	got, _ := kr.DecryptString(ct)
	if got != "hi" {
		t.Errorf("round-trip via base64 keyring failed: %q", got)
	}
	if !bytes.HasPrefix([]byte(ct), []byte("v1:v1:")) {
		t.Errorf("default key_id should be v1")
	}
}
