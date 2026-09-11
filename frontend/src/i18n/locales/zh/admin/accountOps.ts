export default {
  accountOps: {
    title: '账号监控',
    description: '按上游账号或分组查看请求质量、吞吐与告警',
    selectGroup: '选择分组',
    noGroupFilter: '不限定分组',
    selectAccount: '选择账号',
    selectAccountOptional: '选择账号（可选）',
    noAccountFilter: '不限定账号',
    searchAccount: '搜索账号名称或 ID',
    selectAccountHint: '请选择一个账号或分组以加载监控数据',
    accountRequired: '账号监控必须指定一个账号',
    model: '模型',
    timeRange: '时间范围',
    allModels: '全部模型',
    lastUpdated: '刷新于 {time}',
    autoRefresh: '自动刷新',
    trends: {
      sla: 'SLA 趋势',
      pv: 'PV 趋势',
      ttft: 'TTFT P50 / P90 趋势',
      requests: '请求数'
    },
    metrics: {
      requests: '请求', sla: 'SLA', requestErrors: '请求错误', duration: '请求时长', ttft: 'TTFT', upstreamErrors: '上游错误',
      successes: '成功 {count}', tokens: 'Token {count}', errors: '错误 {count}', samples: '样本 {count}', excluded: '排除 429/529',
      percentile: 'P95 {p95} · 平均 {avg}'
    },
    alertScopeHint: '仅展示符合当前账号和分组条件的告警事件。',
    loadFailed: '账号监控数据加载失败'
  }
}
