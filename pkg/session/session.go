// Package session 签发/校验平台自有会话 token(10 §1.4)。
//
// 平台自有会话(非 new-api 的 access_token),客户端统一用
// Authorization: Bearer <platform_session_token> 携带。token 内含
// member_id/org_id/role 及可选的 support_session(运营方支持态);服务端据此判
// 鉴权,绝不信任前端传来的 org_id/role。
//
// 实现:紧凑串 base64url(payloadJSON).base64url(HMAC-SHA256),无需 JWT 库;
// 签名密钥经环境变量注入(与主密钥分离)。这是无状态会话:吊销靠短 TTL +
// (如需)服务端 support_session 状态校验,不在本包内做黑名单。
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrInvalidToken 表示 token 格式错/签名不匹配/被篡改。
	ErrInvalidToken = errors.New("session: token 非法或签名不匹配")
	// ErrExpired 表示 token 已过期(调用方应按 10001/10003 让前端重登录)。
	ErrExpired = errors.New("session: 会话已过期")
)

// Role 是产品角色(09 §14 / 08 §0.1)。
type Role string

const (
	RoleOperator   Role = "operator"    // 运营方(平台,跨全部 org)
	RoleOrgAdmin   Role = "org_admin"   // 组织管理员(本 org 全部,含计费子集)
	RoleTeamLeader Role = "team_leader" // 团队负责人(限本 team + 成员自助)
	RoleMember     Role = "member"      // 成员(仅本人)
)

// Valid 报告角色是否为已知枚举值。
func (r Role) Valid() bool {
	switch r {
	case RoleOperator, RoleOrgAdmin, RoleTeamLeader, RoleMember:
		return true
	}
	return false
}

// Claims 是会话载荷。服务端据此判鉴权;不放任何敏感凭证。
type Claims struct {
	MemberID int64  `json:"mid"`
	OrgID    int64  `json:"oid"`
	Role     Role   `json:"role"`
	TeamID   int64  `json:"tid,omitempty"` // 团队负责人/成员的所辖团队;0=无
	// SupportSessionID 非 0 表示运营方处于支持态(只读/协助),写端点据此叠加闸(08 §2.2)。
	SupportSessionID int64 `json:"sid,omitempty"`
	IssuedAt         int64 `json:"iat"`
	ExpiresAt        int64 `json:"exp"`
}

// Signer 用签名密钥签发/校验 token。
type Signer struct {
	key []byte
	ttl time.Duration
	// nowFn 可注入以便测试;生产用 time.Now。
	nowFn func() time.Time
}

// NewSigner 用签名密钥(原始字节,建议 >=32B)与默认 TTL 构造 Signer。
func NewSigner(key []byte, ttl time.Duration) (*Signer, error) {
	if len(key) < 16 {
		return nil, fmt.Errorf("session: 签名密钥过短(>=16 字节)")
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &Signer{key: k, ttl: ttl, nowFn: time.Now}, nil
}

// Issue 用给定 claims 签发 token,自动填 IssuedAt/ExpiresAt(若未设)。
func (s *Signer) Issue(c Claims) (string, error) {
	now := s.nowFn()
	if c.IssuedAt == 0 {
		c.IssuedAt = now.Unix()
	}
	if c.ExpiresAt == 0 {
		c.ExpiresAt = now.Add(s.ttl).Unix()
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("session: 序列化 claims 失败: %w", err)
	}
	p := b64.EncodeToString(payload)
	sig := s.sign(p)
	return p + "." + sig, nil
}

// Verify 校验签名与有效期,返回 claims。签名错→ErrInvalidToken;过期→ErrExpired。
func (s *Signer) Verify(token string) (Claims, error) {
	var c Claims
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return c, ErrInvalidToken
	}
	expected := s.sign(parts[0])
	// 恒定时间比较,防时序侧信道。
	if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expected)) != 1 {
		return c, ErrInvalidToken
	}
	payload, err := b64.DecodeString(parts[0])
	if err != nil {
		return c, ErrInvalidToken
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, ErrInvalidToken
	}
	if !c.Role.Valid() {
		return c, ErrInvalidToken
	}
	if s.nowFn().Unix() >= c.ExpiresAt {
		return c, ErrExpired
	}
	return c, nil
}

func (s *Signer) sign(payloadB64 string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payloadB64))
	return b64.EncodeToString(mac.Sum(nil))
}

var b64 = base64.RawURLEncoding
