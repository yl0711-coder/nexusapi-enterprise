-- 架构B 阶段1(组长契约增补,33 §12):档位授权 target_type 增 'all'(全组织成员可用,target_id 恒 0)。
-- 统一以 tier_grant 行表达三种授权(all/member/team),DELETE grant 走 {grant_id} 均匀可删;
-- tier.visibility(0032)保留兼容读(两机制在授权反查里都被承认),写路径以 grant 行为准。
-- 回滚:先 DELETE FROM tier_grant WHERE target_type='all'; 再 ALTER TABLE tier_grant MODIFY target_type ENUM('member','team') NOT NULL;
ALTER TABLE tier_grant MODIFY target_type ENUM('all','member','team') NOT NULL;
