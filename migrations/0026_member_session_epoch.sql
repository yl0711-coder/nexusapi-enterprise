-- 迁移 0026(深审 A3):成员会话代次 session_epoch。
-- 平台会话是无状态 HMAC token(默认 12h TTL),原本禁用/降级/改密/硬停后旧 token 仍保全部权限达 12h
-- (角色漂移可自改回、被禁账号可继续操作)。加此列,requireAuth 每请求回查须与库中一致;
-- 禁用/改角色/改密/硬停时自增,令该成员所有旧 token 立即失效。
-- 默认 0:老 token 无 ep 字段解析为 0 → 与默认 0 匹配,部署瞬间不踢线,自然到期后新机制生效。
ALTER TABLE member ADD COLUMN session_epoch INT NOT NULL DEFAULT 0 AFTER status;
