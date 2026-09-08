# 43 基础修复候选（Relay 关闭）

此候选用于独立发布 PR23、PR24、PR26。基线为 `75a7bf274040562c47d951368f22a82d7cc96249`，包含 PR27 的源码发布摘要修复；其 `backend` tree 与 PR26 合并点 `885a13852b8e0247debc9fbde90d1d0b182e49bd` 完全一致，均为 `12476389a95db698de220192e4ee8d3d934839aa`。不包含 PR28 的两项应用修改。

从旧 `77a06d0c8` 只复用组合测试与候选 probe，旧候选没有修改。本候选对基线 backend 唯一增加的是 `error_recovery_encrypted_integration_test.go`；没有进一步改应用实现。

本地验证：组合恢复及 SLA 来源目标回归通过（service 1.348s、handler 4.310s），service/handler 两包编译通过；probe 的 15 个离线安全/预期用例通过。PR26 的既有回归与 race 已在独立 PR 中完成，本轮不重复扩展测试矩阵。实际候选二进制/真实链路门禁仍须由发布 owner 执行，当前文档不证明生产上线或性能收益。

## 发布与直接功能门禁

发布 owner 通过既有审计 wrapper 设置 `SUB2API_RELEASE_RELAY_ENABLED=false`，使用本候选实际 HEAD/tag。不要通过 `inherit` 意外继承上一实验的 Relay 开启配置。发布、回滚和 fleet lease 均由主任务独占执行。

完成 0% stage 并核对候选 image/binary 后，在目标 43 上使用已复制的候选 probe：

```sh
python3 /path/to/hk43_gpt_candidate_probe.py \
  --slot CANDIDATE_SLOT \
  --expect-image VERIFIED_IMAGE_ID \
  --expect-binary-sha256 VERIFIED_BINARY_SHA256 \
  --release-owner CURRENT_FLEET_LEASE_OWNER \
  --model gpt-6-astra \
  --require-relay-disabled \
  --execute-isolated-fixtures
```

将大写值替换为本次 stage 的已验证值。`--require-relay-disabled` 在创建 fixture 前要求容器明确配置 `GATEWAY_TEAMO_RELAY_ENABLED=false`。脚本仍只读全局错误规则，不修改生产账号、Key、规则或 Relay 配置；临时 fixture 只加入自身独占分组。该工具不替代真正 43→101 的原 Key/模型/协议门禁。

## 新增 PR26 入库门禁

保留原五个故障/恢复场景，增加两个已有场景的实际数据库核验，不增加生产请求：

- `already_output`：仅一条最终失败，`is_business_limited=false`，来源为 `upstream/provider/upstream_http`，上游状态 502，fixture ordinal 0 的真实终态事件存在。其 client 逻辑状态目前可为 503，不能用该值冒充真实上游状态或反过来替代。
- `bare_sse_fallback`：仅一条 2xx 恢复记录，client 失败数为 0；上游错误仍记在失败的 ordinal 0 上，不要求顶层账号等于成功接住请求的 B。

关联使用响应 `X-Client-Request-ID` 与本轮单次使用的 fixture key/group，不能自行生成请求 header 后假定命中。只读 SQL 投影包含状态、来源、fixture ordinal 与匹配布尔值；不输出请求标识、prompt、原始错误正文或凭据。每次查询 `BEGIN READ ONLY`、statement timeout 2 秒，最多返回 8 条；异步等待上限约 30 秒且需 5 秒稳定期。全部场景结束后在清理前再做一次验证；未落库、重复终态、被错误排除或来源缺失均使 gate 失败，不重发请求或修改规则以求通过。

证据仍写入 `/opt/sub2api/deploy/rollout/runtime/gpt-candidate-<随机值>.json`。PR26 是统计口径修复，必须与实际降错收益分开记录；生产收益需另取正式可比窗口验证。
