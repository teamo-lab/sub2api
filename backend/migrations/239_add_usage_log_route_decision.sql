-- 选路实验（影子 / A/B）的逐请求决策：组别、会话短哈希、实际选中的号、新逻辑首选、
-- 各候选号的预计首字/罚分/成本。只有参与实验的分组写入，其余为 NULL。
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS route_decision JSONB;

COMMENT ON COLUMN usage_logs.route_decision IS
    'OpenAI route experiment decision (arm, session hash, picked/best account, per-candidate estimates); NULL when not in an experiment group';
