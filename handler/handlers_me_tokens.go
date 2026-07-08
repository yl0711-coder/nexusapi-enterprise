// 架构B 阶段1 · 成员自助令牌端点(33 §3.5 member 组:/me/tokens*)。
// RBAC 铁律:member-only(service 层 requireSelfServiceMember 强制);/me 维度天然本人,无跨成员 id 可传。
// 金额字段 API 层一律 *_raw(int64),FE 用 platform-settings.quota_per_unit 换算美元显示(33 §3.5 通用约定)。
package handler

import (
	"fmt"
	"net/http"

	"github.com/nexusapi-platform/enterprise/service"
)

// GET /me/tokens — 本人令牌列表(名/脱敏 key/分组/额度/剩余/IP/状态/用量)。
func (h *Handler) handleListMyTokens(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	tokens, limit, err := h.svc.ListMyTokens(r.Context(), c)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	// token_limit 固定回传(组长契约增补 33 §12-③,FE 据此画「N/上限」)。
	writeOK(w, r, http.StatusOK, map[string]any{"list": tokens, "token_limit": limit})
}

type createMyTokenReq struct {
	Name     string `json:"name"`
	Group    string `json:"group"`
	QuotaRaw *int64 `json:"quota_raw"` // 可选令牌额度(raw);缺省=成员额度帽(仍 finite)
	AllowIPs string `json:"allow_ips"`
}

// POST /me/tokens — 建令牌(分组限被授权档位;token 必 finite;上限事务内 count+insert)。不回明文 key。
func (h *Handler) handleCreateMyToken(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	var in createMyTokenReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := h.svc.CreateMyToken(r.Context(), c, service.CreateMyTokenInput{
		Name: in.Name, Group: in.Group, QuotaRaw: in.QuotaRaw, AllowIPs: in.AllowIPs,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, v)
}

type updateMyTokenReq struct {
	Group    *string `json:"group"`
	QuotaRaw *int64  `json:"quota_raw"`
	AllowIPs *string `json:"allow_ips"`
}

// PATCH /me/tokens/{id} — 改分组(限授权)/令牌额度/IP;key 与名不可改(换 key=删了重建)。
func (h *Handler) handleUpdateMyToken(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	tokenID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in updateMyTokenReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.UpdateMyToken(r.Context(), c, tokenID, service.UpdateMyTokenInput{
		Group: in.Group, QuotaRaw: in.QuotaRaw, AllowIPs: in.AllowIPs,
	}); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"token_id": tokenID, "updated": true})
}

// DELETE /me/tokens/{id} — 删令牌(归属账 append-only 置 revoked,历史归因不丢)。
func (h *Handler) handleDeleteMyToken(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	tokenID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.DeleteMyToken(r.Context(), c, tokenID); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"token_id": tokenID, "deleted": true})
}

// POST /me/tokens/{id}/key:reveal — 揭示明文(仅本人;挡支持态;明文只即时回传,不落库不写日志)。
// 路径含 key: 天然命中红线能力白名单的默认拒(支持态 10403)。
// B3 镜像 new-api GetFullKey 口径补齐(总监裁定,不自造):CriticalRateLimit(按成员身份限频,
// 20 次/20 分钟)+ 响应禁缓存(no-store——明文 key 绝不允许被浏览器/中间层缓存)。
func (h *Handler) handleRevealMyTokenKey(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	tokenID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if !h.critLim.gate(w, r, fmt.Sprintf("reveal:%d:%d", c.OrgID, c.MemberID)) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	key, err := h.svc.RevealMyTokenKey(r.Context(), c, tokenID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"api_key": key})
}
