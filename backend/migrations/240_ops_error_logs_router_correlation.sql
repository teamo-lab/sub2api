-- 前置 TeamoRouter 在转发时声明的 router request_id 与终端用户 uid
-- （X-Teamo-Router-Request-Id / X-Teamo-Router-User-Id）。Sub2API 的 user_id 对 router 流量
-- 恒为 router 自己的 key，靠这两列才能把错误对回具体用户的具体 router 请求。
-- 只做排障关联展示，不参与鉴权/计费；历史行与非 router 流量为 NULL。
ALTER TABLE ops_error_logs
    ADD COLUMN IF NOT EXISTS router_request_id VARCHAR(64),
    ADD COLUMN IF NOT EXISTS router_user_id VARCHAR(64);
