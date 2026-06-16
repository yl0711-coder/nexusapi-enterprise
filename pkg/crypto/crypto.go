// Package crypto 实现平台敏感字段的静态加密(10 §3)。
//
// 算法 AES-256-GCM(认证加密 + 完整性校验);每条记录独立 96-bit 随机 nonce
// (GCM nonce 复用会致命泄露,绝不复用)。密文统一单字符串格式,便于存/迁移:
//
//	v1:<key_id>:<base64(nonce)>:<base64(ciphertext+gcm_tag)>
//
//   - v1       格式版本
//   - key_id   加密所用主密钥版本(支持轮换共存,10 §3.3):解密按密文里的 key_id
//     选对应密钥(旧的仍能解),加密一律用最新 key。
//
// 主密钥经环境变量注入(现环境无 KMS,10 §3.2),base64 的 32 字节;
// 绝不入镜像/代码库/日志/备份明文。加密对象:member.access_token、
// member.member_password(重 bootstrap 兜底)。明文 API key 不长期落库。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// formatVersion 是密文串的格式版本前缀。
const formatVersion = "v1"

// KeySize 是 AES-256 主密钥的字节长度。
const KeySize = 32

var (
	// ErrEmptyKeyring 表示未配置任何主密钥(进程不应在此状态处理密文)。
	ErrEmptyKeyring = errors.New("crypto: 未配置主密钥(NEXUS_MASTER_KEY 缺失)")
	// ErrBadCiphertext 表示密文串格式非法或被篡改(GCM 校验失败)。
	ErrBadCiphertext = errors.New("crypto: 密文格式非法或完整性校验失败")
	// ErrUnknownKeyID 表示密文引用了一个钥匙环中不存在的 key_id(可能旧密钥已下线)。
	ErrUnknownKeyID = errors.New("crypto: 密文引用了未知的主密钥版本")
)

// Keyring 持有一组主密钥(按 key_id 索引),支持轮换期新旧并存。
// 加密一律用 currentID 对应的密钥;解密按密文里的 key_id 选钥。
type Keyring struct {
	keys      map[string][]byte // key_id -> 32 字节密钥
	currentID string            // 当前用于加密的 key_id(最新)
}

// NewKeyring 用 currentID 指定的当前密钥构造钥匙环。
// keys 形如 {"v1": <32B>, "v2": <32B>};currentID 必须在 keys 中存在。
func NewKeyring(currentID string, keys map[string][]byte) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, ErrEmptyKeyring
	}
	cur, ok := keys[currentID]
	if !ok {
		return nil, fmt.Errorf("crypto: current key_id %q 不在密钥集合内", currentID)
	}
	if len(cur) != KeySize {
		return nil, fmt.Errorf("crypto: 主密钥长度须为 %d 字节,得到 %d", KeySize, len(cur))
	}
	cp := make(map[string][]byte, len(keys))
	for id, k := range keys {
		if len(k) != KeySize {
			return nil, fmt.Errorf("crypto: 主密钥 %q 长度须为 %d 字节,得到 %d", id, KeySize, len(k))
		}
		b := make([]byte, len(k))
		copy(b, k)
		cp[id] = b
	}
	return &Keyring{keys: cp, currentID: currentID}, nil
}

// NewKeyringFromBase64 从 base64 字符串构造单密钥钥匙环(MVP 常用:环境变量注入一把)。
// keyID 缺省 "v1"。
func NewKeyringFromBase64(keyID, b64 string) (*Keyring, error) {
	if keyID == "" {
		keyID = formatVersion
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("crypto: 主密钥 base64 解码失败: %w", err)
	}
	return NewKeyring(keyID, map[string][]byte{keyID: raw})
}

// CurrentKeyID 返回当前用于加密的 key_id。
func (kr *Keyring) CurrentKeyID() string { return kr.currentID }

// Encrypt 用当前密钥加密明文,返回统一格式密文串。每次调用生成独立随机 nonce。
func (kr *Keyring) Encrypt(plaintext []byte) (string, error) {
	key := kr.keys[kr.currentID]
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto: 生成 nonce 失败: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	return strings.Join([]string{
		formatVersion,
		kr.currentID,
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(ct),
	}, ":"), nil
}

// EncryptString 是 Encrypt 的字符串便捷封装。
func (kr *Keyring) EncryptString(s string) (string, error) { return kr.Encrypt([]byte(s)) }

// Decrypt 解析并解密统一格式密文串,按其 key_id 选钥。
func (kr *Keyring) Decrypt(s string) ([]byte, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 || parts[0] != formatVersion {
		return nil, ErrBadCiphertext
	}
	keyID := parts[1]
	key, ok := kr.keys[keyID]
	if !ok {
		return nil, ErrUnknownKeyID
	}
	nonce, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrBadCiphertext
	}
	ct, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, ErrBadCiphertext
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, ErrBadCiphertext
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// GCM 校验失败:密文被篡改或用错密钥。不泄露细节。
		return nil, ErrBadCiphertext
	}
	return pt, nil
}

// DecryptString 是 Decrypt 的字符串便捷封装。
func (kr *Keyring) DecryptString(s string) (string, error) {
	b, err := kr.Decrypt(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// KeyIDOf 返回密文串使用的 key_id(供轮换 worker 判断是否需重加密),不解密。
func KeyIDOf(s string) (string, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 || parts[0] != formatVersion {
		return "", false
	}
	return parts[1], true
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: 初始化 AES 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: 初始化 GCM 失败: %w", err)
	}
	return gcm, nil
}
