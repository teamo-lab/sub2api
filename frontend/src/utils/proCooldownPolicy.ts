export const cooldownDefaults = {
  enabled: false, p90_threshold_seconds: 20, baseline_multiplier: 2,
  baseline_p90_seconds: 5, min_samples: 30, consecutive_windows: 2,
  window_minutes: 30, min_sessions: 3, session_p90_seconds: 20,
  cooldown_minutes: 30, recheck_minutes: 90, minimum_remaining_slots: 9,
  max_capacity_utilization: 0.75, max_error_rate: 0.05, min_safety_samples: 100,
  max_load_per_cpu: 0.7, max_memory_used: 0.8, min_disk_free: 0.2,
  require_second_ip_verification: true
}
export type CooldownPolicy = typeof cooldownDefaults & Record<string, unknown>
export const cooldownFields = [
  { key: 'p90_threshold_seconds', label: 'P90 绝对阈值（秒）', min: 20, max: 120, step: 1 },
  { key: 'baseline_multiplier', label: '相对基线倍数', min: 1.5, max: 10, step: 0.1 },
  { key: 'baseline_p90_seconds', label: '健康基线 P90（秒）', min: 1, max: 120, step: 0.001 },
  { key: 'min_samples', label: '每窗最少有效样本', min: 30, max: 1000, step: 1 },
  { key: 'consecutive_windows', label: '连续恶化窗口数', min: 2, max: 6, step: 1 },
  { key: 'min_sessions', label: '需要同时高尾的 session 数', min: 3, max: 10, step: 1 },
  { key: 'session_p90_seconds', label: 'Session P90 阈值（秒）', min: 20, max: 120, step: 1 },
  { key: 'recheck_minutes', label: '两次冷却最小间隔（分钟）', min: 90, max: 360, step: 1 },
  { key: 'minimum_remaining_slots', label: '冷却后同模型最少剩余并发', min: 9, max: 150, step: 1 },
  { key: 'max_capacity_utilization', label: '剩余容量使用率上限（0–1）', min: 0.1, max: 0.75, step: 0.01 },
  { key: 'max_error_rate', label: '最终 5xx / 429 占比上限（0–1）', min: 0, max: 0.05, step: 0.001 },
  { key: 'min_safety_samples', label: '可用性检查最少样本', min: 100, max: 10000, step: 1 },
  { key: 'max_load_per_cpu', label: '每核系统负载上限', min: 0.1, max: 0.7, step: 0.05 },
  { key: 'max_memory_used', label: '内存使用率上限（0–1）', min: 0.1, max: 0.8, step: 0.05 },
  { key: 'min_disk_free', label: '磁盘空闲比例下限（0–1）', min: 0.2, max: 0.9, step: 0.05 }
] as const
export function readCooldownPolicy(raw: unknown): CooldownPolicy {
  return { ...cooldownDefaults, ...(raw && typeof raw === 'object' && !Array.isArray(raw) ? raw : {}) }
}
export function validateCooldownPolicy(policy: CooldownPolicy): string | null {
  if (typeof policy.enabled !== 'boolean') return '冷却策略开关无效'
  if (policy.window_minutes !== 30 || policy.cooldown_minutes !== 30) return '当前策略的观察窗口和冷却时长固定为 30 分钟'
  for (const field of cooldownFields) {
    const value = policy[field.key]
    if (typeof value !== 'number' || !Number.isFinite(value) || value < field.min || value > field.max || (field.step === 1 && !Number.isInteger(value))) {
      return `${field.label}须在 ${field.min}–${field.max} 之间${field.step === 1 ? '，且为整数' : ''}`
    }
  }
  if (typeof policy.require_second_ip_verification !== 'boolean') return '第二 IP 验证设置无效'
  return null
}
