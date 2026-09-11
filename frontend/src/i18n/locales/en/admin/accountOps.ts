export default {
  accountOps: {
    title: 'Account Monitoring',
    description: 'Request quality, throughput, and alerts scoped to an upstream account',
    selectAccount: 'Select account',
    searchAccount: 'Search account name or ID',
    selectAccountHint: 'Select an account to load monitoring data',
    accountRequired: 'Account monitoring requires an account',
    model: 'Model',
    timeRange: 'Time range',
    allModels: 'All models',
    lastUpdated: 'Updated at {time}',
    autoRefresh: 'Auto refresh',
    metrics: {
      requests: 'Requests', sla: 'SLA', requestErrors: 'Request errors', duration: 'Request duration', ttft: 'TTFT', upstreamErrors: 'Upstream errors',
      successes: '{count} successful', tokens: '{count} tokens', errors: '{count} errors', samples: '{count} samples', excluded: 'Excludes 429/529',
      percentile: 'P95 {p95} · Avg {avg}'
    },
    alertScopeHint: 'Only new alert events carrying this account dimension are shown.',
    loadFailed: 'Failed to load account monitoring data'
  }
}
