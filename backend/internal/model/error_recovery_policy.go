package model

// ErrorRecoveryPolicy is opt-in; absent/default preserves legacy behavior.
type ErrorRecoveryPolicy struct {
	Mode               string   `json:"mode"`
	AccountTypes       []string `json:"account_types"`
	Models             []string `json:"models"`
	UpstreamCodes      []string `json:"upstream_codes"`
	SameAccountRetries int      `json:"same_account_retries"`
	AccountSwitches    int      `json:"account_switches"`
	BudgetSeconds      int      `json:"budget_seconds"`
}

func (p *ErrorRecoveryPolicy) Validate() error {
	bad := func(s string) error { return &ValidationError{Field: "recovery_policy", Message: s} }
	if p.Mode != "default" && p.Mode != "return" && p.Mode != "limited" {
		return bad("invalid recovery mode")
	}
	if p.Mode == "default" {
		return nil
	}
	if p.SameAccountRetries < 0 || p.SameAccountRetries > 10 || p.AccountSwitches < 0 || p.AccountSwitches > 10 {
		return bad("retry and switch limits must be between 0 and 10")
	}
	if p.BudgetSeconds < 1 || p.BudgetSeconds > 120 {
		return bad("recovery budget must be between 1 and 120 seconds")
	}
	for _, t := range p.AccountTypes {
		if t != "oauth" && t != "apikey" {
			return bad("account type must be oauth or apikey")
		}
	}
	if len(p.UpstreamCodes) == 0 {
		return bad("at least one structured upstream error code is required")
	}
	for _, code := range p.UpstreamCodes {
		if code == "" {
			return bad("empty upstream error code")
		}
	}
	return nil
}
