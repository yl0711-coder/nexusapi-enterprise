package service

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

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
// 40号 P3-3:rand.Read 显式检查(与本文件其余 rand.Read 口径一致;虽非密钥场景,
// 但吞错写法易被复制到真密钥场景)。失败回退时间戳后缀(仅邮箱占位,唯一性由 DB 约束兜)。
func randEmailSuffix() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
}

// deriveOrgUsername 【已弃用·仅遗留兜底】旧的确定性 org 用户名(org<id>)。v1.1 项B 换成 genOrgUsername 随机名存库
// (共享 new-api 上 org<id> 可猜/会撞主站客户)。仅在读到 NULL newapi_username 的遗留组织时兜底(灰度清库后不该出现)。
func deriveOrgUsername(orgID int64) string {
	return fmt.Sprintf("org%d", orgID)
}

// genOrgUsername v1.1 项B:门A 组织 new-api 用户名——高熵随机名。`ent_` 前缀(可辨识是平台建的)+ base32 随机,
// 总长 ≤ 20(满足 rc.4 username 约束)。首次 provision 生成、落库;意外撞名(概率 ~2^-80)由调用方有界重生成兜。
func genOrgUsername() (string, error) {
	b := make([]byte, 10) // 10 字节 = 16 个 base32 字符,80 位熵
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return "ent_" + suffix, nil // 4 + 16 = 20 字符
}

// deriveTokenName 派生 new-api 侧确定性 token name(10 §2.5),与 adapter 内部一致。
func deriveTokenName(memberID int64, rotation int) string {
	return fmt.Sprintf("nexus_m%d_v%d", memberID, rotation)
}
