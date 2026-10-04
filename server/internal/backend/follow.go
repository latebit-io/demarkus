package backend

// Follower is a store several replicas share: each replica tells the others
// the sequences it learns the store committed through, and Follow passes a
// peer's on. Contract: mark://soul.demarkus.io/adr/0036-bucket-store-commit-log.md
type Follower interface {
	// Follow says a peer learned the store committed through sequence: the
	// store reads ahead unless it serves it, and may share writes fairly by
	// it. A store whose reads are always current ignores it. Never blocks.
	Follow(sequence int64)
}
