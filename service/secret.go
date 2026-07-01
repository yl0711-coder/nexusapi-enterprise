package service

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// genNewapiPassword 生成满足 new-api 约束(8-20 字符、含字母/数字/特殊)的随机密码。
// 用于平台为成员在 new-api 侧建用户(平台生成并加密留存,重 bootstrap 兜底,10 §2.7)。
func genNewapiPassword() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// base64 → 12 字符随机体(字母/数字/+/),补固定后缀保证含各字符类且总长 16(<=20)。
	body := strings.NewReplacer("+", "A", "/", "a", "=", "z").Replace(base64.StdEncoding.EncodeToString(b))
	return "Px" + body + "9!", nil
}

// genPlatformPassword 生成平台登录初始密码(随机,12 字符)。
func genPlatformPassword() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.NewReplacer("+", "X", "/", "x", "=", "Z").Replace(base64.StdEncoding.EncodeToString(b)), nil
}

// hashPassword 用 bcrypt 单向哈希平台登录密码(存 member.platform_password_hash)。
func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// checkPassword 校验明文与 bcrypt 哈希。
func checkPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// maskKey 生成脱敏 key 串供列表/详情展示(10 §3.1:明文只创建/轮换一次回显)。
// 形如 sk-nexus-••••e93f:保留可辨识的前缀与末 4 位,中间打码。
func maskKey(plaintext string) string {
	const dots = "••••"
	if len(plaintext) <= 8 {
		return dots
	}
	head := plaintext[:8]
	tail := plaintext[len(plaintext)-4:]
	return head + dots + tail
}

// randEmailSuffix 生成 8 位随机后缀(自动派生 login_email 用)。
func randEmailSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
}

// deriveOrgUsername 模型2:组织 new-api user 的确定性 username(幂等键,adopt-existing 防重复建)。
// 与成员名(o{}m{})不撞:org 名恒带 "org" 前缀、无 'm' 段。<=20。
func deriveOrgUsername(orgID int64) string {
	return fmt.Sprintf("org%d", orgID)
}

// deriveTokenName 派生 new-api 侧确定性 token name(10 §2.5),与 adapter 内部一致。
func deriveTokenName(memberID int64, rotation int) string {
	return fmt.Sprintf("nexus_m%d_v%d", memberID, rotation)
}
