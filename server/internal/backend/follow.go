package backend

// Follower is a store several replicas share. Each replica tells the others
// the sequence of its own commits (the ChangeSource sequence), and Follow
// passes a peer's on. Contract: mark://soul.demarkus.io/adr/0036-bucket-store-commit-log.md
type Follower interface {
	// Follow says a peer replica committed through sequence: the store reads
	// ahead of its own schedule unless it already serves that sequence. A
	// store whose reads are always current ignores it. It never blocks.
	Follow(sequence int64)
}
