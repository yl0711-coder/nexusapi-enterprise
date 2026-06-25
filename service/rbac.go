package service

import (
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// RBAC 校验(08 §2 矩阵 + 10 §1.4)。统一在 service 层强制、默认拒绝。
//
// 关键区分(08 §0.2):
//   - 跨 org 访问他组织资源 → 404(不暴露存在性),由 assertOrgScope 返 NotFound。
//   - 同 org 内越本团队范围(团队负责人)→ 403,由 assertTeamScope 返 Forbidden。
//   - 角色不够 → 403。
//
// 运营方(operator)对客户组织的常规管理端点默认 403*(08 §2.1 脚注):要碰客户
// 资源必须经支持会话(本里程碑不实现支持态写,仅实现身份/范围闸,支持态写在后续里程碑)。

// assertRole 要求调用者角色在 allowed 内,否则 403。
func assertRole(c session.Claims, allowed ...session.Role) error {
	for _, r := range allowed {
		if c.Role == r {
			return nil
		}
	}
	return apperr.Forbidden("当前角色无权执行该操作")
}

// assertOrgScope 校验调用者可访问目标 org:
//   - operator:可跨 org(返回 nil)——但常规客户管理端点另由 assertRole 限制(operator 不在 allowed 内即被挡)。
//   - 其余角色:只能访问本人会话所属 org;访问他 org → 404(不暴露存在性,08 §0.2)。
func assertOrgScope(c session.Claims, targetOrgID int64) error {
	if c.Role == session.RoleOperator {
		return nil
	}
	if c.OrgID != targetOrgID {
		return apperr.NotFound("资源不存在")
	}
	return nil
}

// mvpHidePrice:MVP(观测)下,客户角色不得读带价/带钱端点(余额/计费设置/折扣倍率/入账记录)。
// 前端已藏 UI,这里堵客户持自己门户登录直连 API 拿价(放行前必做1·财务P0,"藏价"产品意图)。
// 放行:运营方(Role==operator)与运营方支持态(SupportSessionID!=0,token 角色虽是 org_admin 但确为运营方)——看价正常。
func (s *Service) mvpHidePrice(c session.Claims) error {
	if s.mvpPriceHidden(c) {
		return apperr.NotFound("资源不存在")
	}
	return nil
}

// mvpPriceHidden 是"该调用者在 MVP 观测下应被藏价"的单一真值来源:客户角色(非运营方、非支持态)。
// mvpHidePrice 用它做整端点 404;字段级裁剪(如 ListBillingGroups 只剥 ratio 保留分组名/模型集)直接复用它,
// 保证"谁该藏价"两种处置同一判定,不漂移。
func (s *Service) mvpPriceHidden(c session.Claims) bool {
	return s.observeMode && c.Role != session.RoleOperator && c.SupportSessionID == 0
}

// assertTeamScope 校验团队负责人只能作用于本团队成员(同 org 内越团队 → 403)。
// org_admin / operator 不受团队边界约束;成员只能作用于本人(由调用方另判)。
func assertTeamScope(c session.Claims, targetTeamID *int64) error {
	if c.Role != session.RoleTeamLeader {
		return nil
	}
	// 团队负责人:目标必须属其所辖团队。
	if targetTeamID == nil || c.TeamID == 0 || *targetTeamID != c.TeamID {
		return apperr.Forbidden("超出本团队管理范围")
	}
	return nil
}

// assertSelf 校验"成员自助"类端点:成员只能操作本人(E21-E23)。
// 上级角色(team_leader/org_admin/operator)对自己同样允许;对他人由各端点的 scope 判定。
func assertSelf(c session.Claims, targetMemberID int64) error {
	if c.Role == session.RoleMember && c.MemberID != targetMemberID {
		return apperr.Forbidden("成员只能操作本人资源")
	}
	return nil
}
