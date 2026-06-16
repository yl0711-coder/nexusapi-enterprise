package session

import (
	"strings"
	"testing"
	"time"
)

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner([]byte("test-signing-key-at-least-16-bytes"), time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return s
}

func TestIssueVerify_RoundTrip(t *testing.T) {
	s := newTestSigner(t)
	in := Claims{MemberID: 7, OrgID: 3, Role: RoleOrgAdmin, TeamID: 0}
	tok, err := s.Issue(in)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	out, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if out.MemberID != 7 || out.OrgID != 3 || out.Role != RoleOrgAdmin {
		t.Errorf("claims mismatch: %+v", out)
	}
	if out.IssuedAt == 0 || out.ExpiresAt == 0 {
		t.Errorf("iat/exp should be auto-filled: %+v", out)
	}
}

func TestVerify_TamperedSignature(t *testing.T) {
	s := newTestSigner(t)
	tok, _ := s.Issue(Claims{MemberID: 1, OrgID: 1, Role: RoleMember})
	parts := strings.Split(tok, ".")
	// 篡改 payload 但保留旧签名 → 签名不匹配。
	forged := parts[0] + "x." + parts[1]
	if _, err := s.Verify(forged); err != ErrInvalidToken {
		t.Errorf("tampered payload should fail ErrInvalidToken, got %v", err)
	}
}

func TestVerify_WrongKey(t *testing.T) {
	s1 := newTestSigner(t)
	s2, _ := NewSigner([]byte("a-totally-different-signing-key!!"), time.Hour)
	tok, _ := s1.Issue(Claims{MemberID: 1, OrgID: 1, Role: RoleMember})
	if _, err := s2.Verify(tok); err != ErrInvalidToken {
		t.Errorf("token signed by other key should fail, got %v", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	s := newTestSigner(t)
	base := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return base }
	tok, _ := s.Issue(Claims{MemberID: 1, OrgID: 1, Role: RoleMember})
	// 时间快进到 TTL 之后。
	s.nowFn = func() time.Time { return base.Add(2 * time.Hour) }
	if _, err := s.Verify(tok); err != ErrExpired {
		t.Errorf("token should be expired, got %v", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	s := newTestSigner(t)
	for _, bad := range []string{"", "nodot", "a.b.c", ".sig", "payload."} {
		if _, err := s.Verify(bad); err == nil {
			t.Errorf("malformed token %q should fail", bad)
		}
	}
}

func TestVerify_InvalidRole(t *testing.T) {
	s := newTestSigner(t)
	// 手工签一个非法角色的 token:用 Issue 无法造,直接构造 payload。
	// 通过把 Role 设为未知值,Verify 应拒。
	tok, _ := s.Issue(Claims{MemberID: 1, OrgID: 1, Role: Role("superuser")})
	if _, err := s.Verify(tok); err != ErrInvalidToken {
		t.Errorf("unknown role should fail ErrInvalidToken, got %v", err)
	}
}

func TestNewSigner_ShortKey(t *testing.T) {
	if _, err := NewSigner([]byte("short"), time.Hour); err == nil {
		t.Errorf("short key should be rejected")
	}
}
