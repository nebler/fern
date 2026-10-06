package task

// ResultOutcome classifies a sealed result: changed work or an explicit
// no-op against base.
type ResultOutcome string

const (
	ResultChanged   ResultOutcome = "changed"
	ResultNoChanges ResultOutcome = "no_changes"
)
