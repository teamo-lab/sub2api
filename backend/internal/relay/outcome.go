package relay

// Outcome describes transport facts, not a scheduling decision. In particular
// PrecommitFailure does not prove the provider did no work or incurred no cost.
type Outcome string

const (
	Succeeded        Outcome = "succeeded"
	PrecommitFailure Outcome = "precommit_failure"
	CommittedFailure Outcome = "committed_failure"
	Unknown          Outcome = "unknown"
)

// Outcome requires an explicit successful protocol terminal, rather than HTTP
// 200 or EOF alone. An unknown result must not be converted to safe retry.
func (g *CommitGate) Outcome(terminalSuccess, failed bool) Outcome {
	if failed {
		if g.committed {
			return CommittedFailure
		}
		return PrecommitFailure
	}
	if terminalSuccess {
		return Succeeded
	}
	return Unknown
}
