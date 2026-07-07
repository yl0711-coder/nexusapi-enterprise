// 架构B 阶段1 · assertSelf 统一中间件(31-ADR §10 / 30-§8 护栏):
// 成员维度端点(/members/{id}...)统一收敛——成员角色传别人 id 一律 403,不再各端点各写。
// service 层各端点原有的本人判定保留作纵深(漏一处不至于击穿),但归属闸以本中间件为第一道。
package handler

import (
	"net/http"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// memberSelfGuard 包在 requireAuth 之内、业务 handler 之外:member 角色的 {id} ≠ 本人 → 403。
// 上级角色(team_leader/org_admin/operator)不受限,由各端点 RBAC/scope 判定。
func (h *Handler) memberSelfGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, _ := claimsFrom(r.Context())
		if c.Role == session.RoleMember {
			if id, err := pathInt64(r, "id"); err == nil && id != c.MemberID {
				writeErr(w, r, apperr.Forbidden("成员只能操作本人资源"))
				return
			}
		}
		next(w, r)
	}
}
