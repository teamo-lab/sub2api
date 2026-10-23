// 账号售价倍率（sell_rate_multiplier）的表单归一化。
// 后端约定：null/省略 = 不设置；创建时负数等同于不设置；更新时负数 = 清空（回退分组定价）。

/** 表单输入 → 有效售价倍率；空、非数字、负数一律视为未配置（null）。0 合法（免费）。 */
export function normalizeSellRateInput(value: unknown): number | null {
  if (value === null || value === undefined || value === '') return null
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n) || n < 0) return null
  return n
}

/** 创建账号请求体：未配置时省略字段。 */
export function sellRateCreatePayload(value: unknown): number | undefined {
  const n = normalizeSellRateInput(value)
  return n === null ? undefined : n
}

/** 更新账号请求体：未配置时发送 -1，后端据此清空为 NULL。 */
export function sellRateUpdatePayload(value: unknown): number {
  const n = normalizeSellRateInput(value)
  return n === null ? -1 : n
}
