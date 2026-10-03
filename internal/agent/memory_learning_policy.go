package agent

// UnadoptedMemoryLearningPolicy is the learning policy a team configuration
// selects when no memory policy is adopted. Off, observe, and shadow never
// change prompt selection, so configuration alone may choose them. Active
// changes selection and stays behind adoption's review gate, so it runs as
// shadow and the second result reports the downgrade. A configuration without
// a mode keeps the defaults. The runtime and read-only views share this rule
// so they report the mode a run actually uses.
func UnadoptedMemoryLearningPolicy(configured MemoryLearningPolicy) (MemoryLearningPolicy, bool) {
	if configured.Mode == "" {
		return DefaultMemoryLearningPolicy(), false
	}
	if configured.Mode == MemoryLearningActive {
		configured.Mode = MemoryLearningShadow
		return configured, true
	}
	return configured, false
}
