package storetest

import (
	"fmt"
	"math/rand"
	"strings"
)

// CorpusDoc is one generated document with its publisher metadata.
type CorpusDoc struct {
	Path string
	Body string
	Meta map[string]string
}

// corpusWords is the vocabulary the generator draws prose from; technical
// enough that tokenization sees identifiers, paths, and punctuation.
var corpusWords = strings.Fields(`
server client protocol store catalog lookup fetch publish append archive
version hash chain immutable snapshot bucket shard manifest blob token auth
read write scope filter limit query section heading anchor snippet index
term match recall ranking importance title tags metadata frontmatter body
document directory path world broker gateway plugin agent memory knowledge
quic tls udp buffer sysctl kernel socket stream request response status
error retry timeout cancel context worker parallel lock mutex race guard
markdown outline slug fence code list table link image quote emphasis
config flag env default cap budget cost measure benchmark baseline delta
deploy helm chart kind cluster pod probe restart release tag branch commit
merge conflict rebase review test suite conformance differential fuzz
path.Match net.core.rmem_max mark_lookup demarkus-mcp /docs/guide.md
ADR-0012 v0.28.0 read-auth body-match section-index quic-go goldmark
`)

// Corpus returns n deterministic markdown documents shaped like a
// knowledge base: a title, nested sections, prose, lists, and code fences,
// roughly 500 to 4000 bytes each, with tags and importance declared.
func Corpus(n int) []CorpusDoc {
	rng := rand.New(rand.NewSource(1))
	word := func() string { return corpusWords[rng.Intn(len(corpusWords))] }
	sentence := func() string {
		words := make([]string, 6+rng.Intn(14))
		for i := range words {
			words[i] = word()
		}
		return strings.ToUpper(words[0][:1]) + words[0][1:] + " " + strings.Join(words[1:], " ") + "."
	}
	docs := make([]CorpusDoc, n)
	for i := range docs {
		var b strings.Builder
		title := strings.Join([]string{word(), word(), word()}, " ")
		fmt.Fprintf(&b, "# %s\n\n%s %s\n\n", title, sentence(), sentence())
		for s := 0; s < 3+rng.Intn(6); s++ {
			level := "##"
			if s > 0 && rng.Intn(3) == 0 {
				level = "###"
			}
			fmt.Fprintf(&b, "%s %s %s\n\n", level, word(), word())
			for p := 0; p < 1+rng.Intn(3); p++ {
				fmt.Fprintf(&b, "%s %s %s\n\n", sentence(), sentence(), sentence())
			}
			switch rng.Intn(4) {
			case 0:
				fmt.Fprintf(&b, "- %s\n- %s\n- %s\n\n", sentence(), sentence(), sentence())
			case 1:
				fmt.Fprintf(&b, "```bash\n%s %s\n%s\n```\n\n", word(), word(), sentence())
			}
		}
		tags := []string{word(), word(), word()}
		docs[i] = CorpusDoc{
			Path: fmt.Sprintf("/corpus/d%02d/doc-%04d.md", i%40, i),
			Body: b.String(),
			Meta: map[string]string{
				"tags":       strings.Join(tags, ","),
				"importance": fmt.Sprintf("%.1f", float64(rng.Intn(10))/10),
			},
		}
	}
	return docs
}
