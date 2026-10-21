// Package neardtest holds the paragraphs every near-duplicate test scores:
// a lesson, the same lesson reworded by about one word in seven, and an
// unrelated note of the same length.
package neardtest

// Lesson is an 80-token paragraph with a signature.
const Lesson = `The bucket store shards by size, not by count: a shard that
grows past the limit splits on the next checkpoint, and a shard under the
floor merges with its neighbour. Counting documents looked simpler but a
world of small notes and a world of long plans need different widths, and
the size bound keeps every shard fetch under one round trip. The compactor
runs off the writer so a split never blocks a publish; the checkpoint row
carries the source of the write observation so replicas agree on which
version a shard describes.`

// Reworded is Lesson as an agent re-records it: the same claims, other words.
const Reworded = `The bucket store shards by size rather than by count: a shard
growing past the limit is split at the next checkpoint, and one under the
floor is merged with its neighbour. Counting documents seemed simpler, but a
world of small notes and a world of long plans want different widths, and
the size bound keeps each shard fetch inside one round trip. The compactor
runs off the writer so a split never blocks a publish; the checkpoint row
carries the source of the write observation so replicas agree which
version a shard describes.`

// Other is an unrelated paragraph of the same shape.
const Other = `The broker mints a stable token per tenant and pins the
tenant identity to the certificate it saw at join time. A rotated
certificate with the same identity keeps its grants; a new identity under an
old name is refused until an admin re-grants it. The gateway profile decides
which tools a session sees, and the memory profile hides the federation
tools because a soul has one world and nothing to federate with.`

// Documents built from the paragraphs, as the tiers store them.
const (
	LessonDoc   = "# Shards\n\n" + Lesson + "\n"
	RewordedDoc = "# Shard sizing\n\n" + Reworded + "\n"
	OtherDoc    = "# Tenants\n\n" + Other + "\n"
	// HubDoc holds the lesson as one section among others.
	HubDoc = "# Patterns\n\nIntro line.\n\n## Shards\n\n" + Lesson + "\n\n## Tenants\n\n" + Other + "\n"
)
