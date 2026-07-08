// 39号体检阻断-1 补落地:模型广场 + 平台设置两组端点(33 §3.5 契约,此前从未实现,
// 致成员建令牌分组下拉空/org_admin 模型广场页失败/运营方平台设置页(money_freeze 开关)不可达)。
package handler

import (
	"net/http"

	"github.com/nexusapi-platform/enterprise/service"
)

// GET /orgs/{id}/marketplace — 组织模型广场(分组+模型+倍率,O/A)。
func (h *Handler) handleOrgMarketplace(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := h.svc.OrgMarketplace(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, v)
}

// GET /me/marketplace — 成员被授权范围的分组+模型+倍率(建令牌分组下拉同一真值来源)。
func (h *Handler) handleMyMarketplace(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	v, err := h.svc.MyMarketplace(r.Context(), c)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, v)
}

// GET /platform-settings — 平台配置读(operator;其他角色 403,FE 回退 /me 的 quota_per_unit)。
func (h *Handler) handleGetPlatformSettings(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	v, err := h.svc.GetPlatformSettings(r.Context(), c)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, v)
}

// PUT /platform-settings — 平台配置写(operator;部分更新;money_freeze 急停在此翻转,涉钱红开关)。
func (h *Handler) handlePutPlatformSettings(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	var in service.UpdatePlatformSettingsInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := h.svc.UpdatePlatformSettings(r.Context(), c, in)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, v)
}
