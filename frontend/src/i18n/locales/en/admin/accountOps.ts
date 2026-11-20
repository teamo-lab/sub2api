export default {
  accountOps: {
    title: 'Account Monitoring',
    description: 'Request quality, throughput, and alerts scoped to a user, group, or upstream account',
    selectUser: 'Select user',
    selectUserOptional: 'Select user (optional)',
    noUserFilter: 'Any user',
    searchUser: 'Search email, username or ID',
    userTag: 'User #{id}',
    selectGroup: 'Select group',
    noGroupFilter: 'Any group',
    selectAccount: 'Select account',
    selectAccountOptional: 'Select account (optional)',
    noAccountFilter: 'Any account',
    searchAccount: 'Search account name or ID',
    selectAccountHint: 'Select a user, group or account to load monitoring data',
    accountRequired: 'Account monitoring requires an account',
    model: 'Model',
    timeRange: 'Time range',
    allModels: 'All models',
    lastUpdated: 'Updated at {time}',
    autoRefresh: 'Auto refresh',
    trends: {
      sla: 'SLA trend',
      pv: 'PV trend',
      ttft: 'TTFT P50 / P90 trend',
      duration: 'Total duration P50 / P90 trend',
      requests: 'Requests'
    },
    metrics: {
      requests: 'Requests', sla: 'SLA', requestErrors: 'Request errors', duration: 'Request duration', ttft: 'TTFT', upstreamErrors: 'Upstream errors',
      successes: '{count} successful', tokens: '{count} tokens', errors: '{count} errors', samples: '{count} samples', excluded: 'Excludes 429/529',
      percentile: 'P95 {p95} · Avg {avg}'
    },
    breakdown: {
      title: 'SLA by account',
      cardHint: 'Click for per-account view',
      caption: '{count} accounts. Click a header to sort, click an account to open its monitoring',
      columns: { account: 'Account', durationP50: 'Duration P50', durationP90: 'Duration P90', tokens: 'Token usage' },
      pvDetail: '{ok} ok · {err} failed',
      tokenDetail: 'in {input} · out {output} · cache {cache}',
      unassigned: 'No account',
      unassignedHint: 'Failed before an account was scheduled',
      empty: 'No requests in this time range',
      hint: 'SLA = successes ÷ (successes + SLA-counted failures), same as the card above; a request that succeeded after failover counts only on the account that succeeded.',
      latencyDegraded: 'The latency percentile query timed out; only requests and tokens are shown.',
      truncated: 'Too many accounts; showing the {count} with the most requests.',
      loadFailed: 'Failed to load per-account data',
      sorted: 'Sorted by {column}, {direction}, {count} rows',
      ascending: 'ascending',
      descending: 'descending'
    },
    alertScopeHint: 'Only alert events matching the current account and group filters are shown (alerts are not per user).',
    loadFailed: 'Failed to load account monitoring data'
  }
}
