export default {
  accountOps: {
    title: '账号监控',
    description: '按用户、分组或上游账号查看请求质量、吞吐与告警',
    selectUser: '选择用户',
    selectUserOptional: '选择用户（可选）',
    noUserFilter: '不限定用户',
    searchUser: '搜索邮箱、用户名或 ID',
    userTag: '用户 #{id}',
    selectGroup: '选择分组',
    noGroupFilter: '不限定分组',
    selectAccount: '选择账号',
    selectAccountOptional: '选择账号（可选）',
    noAccountFilter: '不限定账号',
    searchAccount: '搜索账号名称或 ID',
    selectAccountHint: '请选择一个用户、分组或账号以加载监控数据',
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
      duration: '总耗时 P50 / P90 趋势',
      requests: '请求数'
    },
    metrics: {
      requests: '请求', sla: 'SLA', requestErrors: '请求错误', duration: '请求时长', ttft: 'TTFT', upstreamErrors: '上游错误',
      successes: '成功 {count}', tokens: 'Token {count}', errors: '错误 {count}', samples: '样本 {count}', excluded: '排除 429/529',
      percentile: 'P95 {p95} · 平均 {avg}'
    },
    breakdown: {
      title: '分账号 SLA',
      cardHint: '点击查看各账号',
      caption: '共 {count} 个账号，点表头排序，点账号名进入该账号监控',
      columns: { account: '账号', durationP50: '总耗时 P50', durationP90: '总耗时 P90', tokens: 'Token 使用' },
      pvDetail: '成功 {ok} · 失败 {err}',
      tokenDetail: '入 {input} · 出 {output} · 缓存 {cache}',
      unassigned: '未分配账号',
      unassignedHint: '调度到账号前就失败的请求',
      empty: '当前时间范围内没有请求',
      hint: 'SLA = 成功 ÷（成功 + 计入 SLA 的失败），口径与上方卡片一致；换号成功的请求只记在最后成功的账号上。',
      latencyDegraded: '延迟分位数查询超时，本次只显示请求数和 Token。',
      truncated: '账号过多，只显示请求量最大的 {count} 个。',
      loadFailed: '分账号数据加载失败',
      sorted: '已按{column}{direction}排序，共 {count} 行',
      ascending: '升序',
      descending: '降序'
    },
    alertScopeHint: '仅展示符合当前账号和分组条件的告警事件（告警不区分用户）。',
    loadFailed: '账号监控数据加载失败'
  }
}
