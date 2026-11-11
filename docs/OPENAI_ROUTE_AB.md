# OpenAI 选路实验（影子模式 + 按分组 A/B）

## 解决什么问题

老选号逻辑是 priority → 负载率 → LRU，不看快慢。某个号变慢但不报错时，新请求还是照样往它身上送，要等人去改 priority。新逻辑对**新会话**按「这一分钟谁更快、又不贵」来选号；老会话（粘性命中）在这一期两组都不迁移。

## 评分

对每个能接这个模型、priority ≤ `max_priority` 的候选号：

```
分数 = 预计首字 + 挂住罚分 + 报错罚分 + 成本
```

- **预计首字**：按「账号 × 档位」记首字的对数均值，按时间衰减（`half_life_sec`）。档位 = 模型 × reasoning effort × 请求体档（s/m/l/x）。预测时拟合「档位基线 + 账号偏移」：
  - 账号只和同档位的其他号比，并且按账号等权。这样「谁接了更重的请求」不会被算成快慢，占大头流量的号变慢时也不会把基线一起拖慢。
  - 样本少的号会向「和大家一样」收缩（`prior_samples`）。
- **挂住罚分**：在途的**流式**请求里，等首字超过 `max(预计 × stall_factor, stall_min_ms)` 的算挂住。罚分 = 挂住占比（分母只算流式在途）× 最长等待时间。请求一挂住就会被计入，不用等它结束。非流式请求不一定有首字事件，只计入在途条数 `n`，不参与挂住判定。
- **报错罚分**：衰减后的失败率 × `error_penalty_ms`。
- **成本**：账号的 `rate_multiplier` × `cost_ms_per_rate`。默认 60000，即 0.05x 约等于 3 秒。

B 组按 `softmax(−分数 / temperature_ms)` 再加上 `explore` 的均匀探索，做无放回抽样，得到尝试顺序。抢槽失败时顺着往下试。priority 大于 `max_priority` 的号排在最后兜底。

## 开关

写在 settings 表的 `openai_route_ab`（JSON）里，5 秒内生效，不用重启：

```sql
INSERT INTO settings (key, value) VALUES ('openai_route_ab',
  '{"groups":{"6":{"mode":"shadow"}},"salt":"nova-2026-11"}')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
```

| mode | 行为 |
|---|---|
| `shadow` | 只算不选。所有请求照旧走老逻辑，`usage_logs.route_decision` 里记下新逻辑会选谁 |
| `ab` | 按「salt + 会话哈希」分桶，`percent`% 的会话进 B 组按新逻辑选，其余进 A 组照旧。两组都记录 |
| 其他值，或分组不在配置里 | 不参与，`route_decision` 为 NULL |

**立刻止损**：把 `percent` 改成 0，或者删掉这个分组。5 秒内全部回到老逻辑。

可调参数及默认值：`half_life_sec` 180、`cost_ms_per_rate` 60000、`temperature_ms` 5000、`explore` 0.03、`max_priority` 2、`stall_factor` 2、`stall_min_ms` 15000、`error_penalty_ms` 30000、`prior_samples` 5。

统计数据在进程内存里，按实例各自记。蓝绿切换之后要几分钟才能重新攒到样本，这段时间评分退化为「成本 + 报错」。

## usage_logs.route_decision

```json
{"arm":"B","layer":"load","sh":"3k9x…","shape":"gpt-6.1-sol|high|m",
 "picked":17,"best":17,"applied":true,
 "est":{"1":{"p":27000,"c":9600,"t":36600,"n":31,"k":4,"s":8000},
        "17":{"p":5200,"c":9000,"t":14200,"n":6}}}
```

- `arm`：S（影子）/ A / B。
- `layer`：sticky（粘性命中，没打分）/ load / fallback。
- `sh`：加盐后的会话短哈希，只用来按会话聚合，不能还原会话键。
- `picked`：实际选中的号。`best`：新逻辑的首选。`applied`：这次是否真的按新逻辑排序。
- `est` 里每个号的字段：p 预计首字、s 挂住罚分、e 报错罚分、c 成本、t 总分、n 在途条数、k 挂住条数（单位都是毫秒）。

## 分析 SQL

影子期，看预测准不准（预计首字 vs 实际首字）：

```sql
SELECT account_id,
       count(*) n,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY first_token_ms) actual_p50,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY (route_decision->'est'->(account_id::text)->>'p')::int) pred_p50
FROM usage_logs
WHERE group_id = 6 AND stream AND route_decision->>'layer' = 'load'
  AND created_at > now() - interval '1 day'
GROUP BY 1;
```

A/B 期，按 10 分钟桶配对比较两组：

```sql
WITH t AS (
  SELECT date_trunc('hour', created_at) + floor(extract(minute FROM created_at)/10) * interval '10 min' b,
         route_decision->>'arm' arm, first_token_ms, duration_ms,
         cache_read_tokens::float / nullif(input_tokens + cache_read_tokens, 0) cache_ratio,
         total_cost * account_rate_multiplier cost
  FROM usage_logs
  WHERE group_id = 6 AND stream AND route_decision->>'arm' IN ('A','B')
    AND created_at > now() - interval '1 day')
SELECT b, arm, count(*),
       percentile_cont(0.9) WITHIN GROUP (ORDER BY first_token_ms) ttft_p90,
       percentile_cont(0.9) WITHIN GROUP (ORDER BY duration_ms) dur_p90,
       avg(cache_ratio) cache, avg(cost) cost
FROM t GROUP BY 1, 2 ORDER BY 1, 2;
```

## 这一期没做

- 老会话迁移（B2 阶段）：粘性命中时两组都不迁移，只记 `layer=sticky`。
- Responses WebSocket 和 Anthropic Messages 入口：选号同样会按 B 组排序，但不记在途、首字和决策。nova 的流量 82% 是 HTTP `/v1/responses`、16% 是 `/v1/chat/completions`，这两条已经覆盖。
