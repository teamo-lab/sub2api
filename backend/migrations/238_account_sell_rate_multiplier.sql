-- 账号售价倍率：非空时按最终服务账号的售价倍率向用户扣费，覆盖分组/用户-分组倍率；
-- 为空（默认）时保持原有分组定价，存量行为零变化。
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS sell_rate_multiplier DECIMAL(10,4);
