package neardup

import "github.com/latebit-io/demarkus/protocol/mdtext"

// Doc is a body's signed texts, for a tier that holds bodies rather than a
// catalog's section index. It is its own Sections.
type Doc struct {
	Whole    Text
	Sections []Text
}

// Analyze tokenizes body by the shared split and signs the whole own text
// and each section, tokens hashed once. Sets are built only for signed texts.
func Analyze(body string) Doc { return analyze(body, true) }

// AnalyzeWhole signs the whole own text alone: what a draft needs.
func AnalyzeWhole(body string) Text { return analyze(body, false).Whole }

func analyze(body string, sections bool) Doc {
	// Under MinTokens kept tokens of at least MinTokenBytes nothing signs.
	if len(body) < MinTokens*MinTokenBytes {
		return Doc{}
	}
	var d Doc
	var whole Signer
	var wholeSet Set
	for _, p := range mdtext.Split(body) {
		counts, _ := mdtext.Counts(mdtext.Strip(p.Own))
		var signer Signer
		var set Set
		for tok, c := range counts {
			if len(tok) < MinTokenBytes {
				continue
			}
			h := Hash(tok)
			signer.Add(h, c)
			set = append(set, h)
		}
		whole.Merge(&signer)
		wholeSet = append(wholeSet, set...)
		if !sections {
			continue
		}
		t := Text{Anchor: p.Anchor, Sig: signer.Sig()}
		if t.Sig != 0 {
			t.Set = set.normalized()
		}
		d.Sections = append(d.Sections, t)
	}
	d.Whole.Sig = whole.Sig()
	if d.Whole.Sig != 0 {
		d.Whole.Set = wholeSet.normalized()
	}
	return d
}

// Len is the number of sections.
func (d *Doc) Len() int { return len(d.Sections) }

// At is section i's anchor and signature.
func (d *Doc) At(i int) Section { return Section{Anchor: d.Sections[i].Anchor, Sig: d.Sections[i].Sig} }

// Set is section i's token set, the whole text's for -1.
func (d *Doc) Set(section int) Set {
	if section < 0 {
		return d.Whole.Set
	}
	return d.Sections[section].Set
}

// Target is the body as a scan target at path.
func (d *Doc) Target(path string) Target { return Target{Path: path, Sig: d.Whole.Sig, Sections: d} }
