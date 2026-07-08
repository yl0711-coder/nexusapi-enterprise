// 架构B 阶段1 BE③(33 §3.3 AttributeLog):消费/日志归因统一收口。
//
// 归因按 **user_id**(成员=各自 new-api user)为主键,并做一致性断言:
//   token→member(member_key_token)与 member.newapi_user_id→org 必须自洽,
//   不一致 → 落**未归因桶**(member_id=0)+ 告警(防跨组织串台,31-ADR §8 护栏)。
//
// 兼容口径(共用生产实例 + 存量 A 版数据):
//   - user 是某组织**金库**(organization.newapi_user_id)且 token→member 属同组织 → A 版遗留
//     (成员令牌挂 org user 下),按 token 归因该成员(org 一致性断言已过);
//   - user 既非成员也非金库 → 主站普通客户日志,跳过。
package service

import (
	"context"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
)

// usageAttribution 一条日志的归因结论。
type usageAttribution struct {
	skip     bool          // 非平台日志(主站客户),整条跳过
	orgID    int64         // 归属组织(未归因桶也有组织)
	member   *model.Member // 归属成员;nil=未归因桶(member_id=0)
	keyID    int64         // 平台稳定 key_id;0=未归因到 key
	mismatch bool          // 一致性断言失败(已按未归因桶归置,调用方告警)
}

// attrCaches 归因查询缓存(单轮结算/镜像内复用,防每行打 DB)。
type attrCaches struct {
	tokenAttr   map[int64]tokenAttr        // token_id -> member/keyID
	userMember  map[int64]*model.Member    // newapi user_id -> member(nil=非成员)
	userOrg     map[int64]int64            // newapi user_id -> 金库所属 org(0=非平台组织)
	mmAlerted   map[int64]bool             // token_id -> 本轮已告警(去抖)
}

func newAttrCaches() *attrCaches {
	return &attrCaches{
		tokenAttr:  map[int64]tokenAttr{},
		userMember: map[int64]*model.Member{},
		userOrg:    map[int64]int64{},
		mmAlerted:  map[int64]bool{},
	}
}

// lookupTokenMember token→(member,keyID) 带缓存。
func (s *Service) lookupTokenMember(ctx context.Context, c *attrCaches, tokenID int64) (tokenAttr, error) {
	if att, ok := c.tokenAttr[tokenID]; ok {
		return att, nil
	}
	mm, kid, found, err := s.store.GetMemberByNewapiTokenID(ctx, tokenID)
	if err != nil {
		return tokenAttr{}, apperr.Internal("").WithCause(err) // DB 错,非上游故障(P1-3:勿误走 mapUpstream)
	}
	att := tokenAttr{found: found, member: mm, keyID: kid}
	c.tokenAttr[tokenID] = att
	return att, nil
}

// lookupUserMember user_id→member 带缓存(nil=非成员)。
func (s *Service) lookupUserMember(ctx context.Context, c *attrCaches, userID int64) (*model.Member, error) {
	if m, ok := c.userMember[userID]; ok {
		return m, nil
	}
	m, found, err := s.store.GetMemberByNewapiUserID(ctx, userID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if !found {
		m = nil
	}
	c.userMember[userID] = m
	return m, nil
}

// lookupUserOrg user_id→金库所属 org 带缓存(0=非平台组织)。
func (s *Service) lookupUserOrg(ctx context.Context, c *attrCaches, userID int64) (int64, error) {
	if id, ok := c.userOrg[userID]; ok {
		return id, nil
	}
	id, found, err := s.store.GetOrgIDByNewapiUserID(ctx, userID)
	if err != nil {
		return 0, apperr.Internal("").WithCause(err)
	}
	if !found {
		id = 0
	}
	c.userOrg[userID] = id
	return id, nil
}

// attributeUsage 归因一条消费日志(userID 主键 + token 一致性断言)。纯决策在 decideAttribution,便于单测。
func (s *Service) attributeUsage(ctx context.Context, c *attrCaches, userID, tokenID int64) (usageAttribution, error) {
	m1, err := s.lookupUserMember(ctx, c, userID)
	if err != nil {
		return usageAttribution{}, err
	}
	var treasuryOrg int64
	if m1 == nil { // 非成员 user:可能是金库(A 版 org user / 金库自身日志)
		treasuryOrg, err = s.lookupUserOrg(ctx, c, userID)
		if err != nil {
			return usageAttribution{}, err
		}
	}
	var tok tokenAttr
	if tokenID > 0 {
		tok, err = s.lookupTokenMember(ctx, c, tokenID)
		if err != nil {
			return usageAttribution{}, err
		}
	}
	return decideAttribution(m1, treasuryOrg, tok), nil
}

// decideAttribution 归因纯决策(无 IO,单测覆盖):
//   A. user=成员 且 token=同一成员         → 归该成员 + key_id
//   B. user=成员 且 token 未登记/无 token  → 归该成员,key_id=0
//   C. user=成员 但 token=**别的成员**     → 断言失败:未归因桶(user 侧组织)+ mismatch
//   D. user=金库 且 token=同组织成员       → A 版遗留:按 token 归该成员(org 断言已过)
//   E. user=金库 但 token=**他组织成员**   → 断言失败:未归因桶(金库组织)+ mismatch(防跨组织串台)
//   F. user=金库 且 token 未登记/无 token  → 未归因桶(门B/金库自身日志,现行为)
//   G. user 非平台                         → skip(主站客户)
func decideAttribution(userMember *model.Member, treasuryOrg int64, tok tokenAttr) usageAttribution {
	if userMember != nil {
		if tok.found {
			if tok.member != nil && tok.member.ID == userMember.ID && tok.member.OrgID == userMember.OrgID {
				return usageAttribution{orgID: userMember.OrgID, member: userMember, keyID: tok.keyID} // A
			}
			return usageAttribution{orgID: userMember.OrgID, mismatch: true} // C
		}
		return usageAttribution{orgID: userMember.OrgID, member: userMember} // B
	}
	if treasuryOrg > 0 {
		if tok.found {
			if tok.member != nil && tok.member.OrgID == treasuryOrg {
				return usageAttribution{orgID: treasuryOrg, member: tok.member, keyID: tok.keyID} // D
			}
			return usageAttribution{orgID: treasuryOrg, mismatch: true} // E
		}
		return usageAttribution{orgID: treasuryOrg} // F
	}
	return usageAttribution{skip: true} // G
}

// alertAttributionMismatch 归因断言失败告警(每 token 每轮一次,防刷屏)。
func (s *Service) alertAttributionMismatch(ctx context.Context, c *attrCaches, orgID, userID, tokenID, logID int64, source string) {
	if c.mmAlerted[tokenID] {
		return
	}
	c.mmAlerted[tokenID] = true
	s.log.Error("归因一致性断言失败:token→member 与 member.newapi_user_id→org 不一致,已落未归因桶(疑跨组织串台,人工核对)",
		"source", source, "org_id", orgID, "user_id", userID, "token_id", tokenID, "log_id", logID)
	oid := orgID
	s.auditSystem(ctx, orgID, "attribution_mismatch", "log_mirror", &oid, map[string]any{
		"source": source, "user_id": userID, "token_id": tokenID, "log_id": logID,
	}, "mismatch")
}
