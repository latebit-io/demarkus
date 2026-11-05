package audit

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/hiddentext"
	"github.com/latebit-io/demarkus/protocol/provenance"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/stylerules"
)

// readPolicy reads the store's own write policy and merges it over the
// floor. Not found is the empty policy; any other failure is a coverage
// note and the policy checks stay off.
func (a *audit) readPolicy(ctx context.Context) {
	resp, err := a.store.Fetch(ctx, publishpolicy.DocumentPath)
	var own publishpolicy.Policy
	switch {
	case err != nil:
		a.note("fetch %s failed: %v; policy checks skipped", publishpolicy.DocumentPath, err)
		return
	case resp.Status == protocol.StatusNotFound:
	case resp.Status != protocol.StatusOK:
		a.note("fetch %s returned %s; policy checks skipped", publishpolicy.DocumentPath, resp.Status)
		return
	default:
		own = publishpolicy.Parse(resp.Body)
	}
	a.policyKnown = true
	a.policy, a.own = own, own
	if a.opts.Floor == nil {
		return
	}
	a.policy = publishpolicy.Merge(*a.opts.Floor, own)
	for _, line := range publishpolicy.Loosens(*a.opts.Floor, own) {
		a.add(CheckPolicyFloor, publishpolicy.DocumentPath, line+"; the floor still applies", "tighten the world policy to the floor or drop the line")
	}
}

// checkPolicy judges every live document's metadata as a publish would,
// under the effective policy. Missing tags are the Untagged check's.
func (a *audit) checkPolicy() {
	if !a.policyKnown {
		return
	}
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		for _, v := range a.violations(p, d.meta, d.body) {
			detail, fix := a.violation(v, d.meta)
			a.add(CheckPolicy, p, detail, fix)
		}
	}
}

// violations is what the store's policy holds against metadata and body at
// path; none when the policy is unknown.
func (a *audit) violations(p string, meta map[string]string, body string) []publishpolicy.Violation {
	if !a.policyKnown {
		return nil
	}
	effective := publishpolicy.Effective(a.opts.Floor, a.own, p)
	return slices.DeleteFunc(publishpolicy.EvaluateBody(effective.ForCaller(), p, meta, body).Violations, func(v publishpolicy.Violation) bool {
		return v.Code == publishpolicy.MissingTags
	})
}

// violation words one violation as a finding's detail and fix.
func (a *audit) violation(v publishpolicy.Violation, meta map[string]string) (detail, fix string) {
	strictness := a.policy.EffectiveStrictness()
	switch v.Code {
	case publishpolicy.InvalidImportance:
		return fmt.Sprintf("importance %q is not a number in [0,1] (strictness %s)", meta["importance"], strictness), "re-publish with a valid metadata.importance"
	case publishpolicy.MissingTagAxis:
		return fmt.Sprintf("missing required tag axis %s: (strictness %s)", v.Name, strictness), "re-publish with a " + v.Name + ":<value> tag"
	case publishpolicy.MissingField:
		return fmt.Sprintf("missing required metadata %s (strictness %s)", v.Name, strictness), "re-publish with metadata." + v.Name
	case publishpolicy.UnpinnedSource:
		return fmt.Sprintf("%s value %s is not a versioned permalink (strictness %s)", provenance.Key, v.Name, strictness), "pin " + provenance.PinForms
	case publishpolicy.DisallowedTagValue:
		return fmt.Sprintf("tag %s is outside the allowed values for %s (%s) (strictness %s)",
			v.Name, v.Axis, a.policy.AllowedList(v.Axis), strictness), "re-publish with an allowed " + v.Axis + ": value"
	case publishpolicy.DisallowedLinkDomain:
		return fmt.Sprintf("links to %s, outside allow_domains (%s) (strictness %s)", v.Name, a.policy.AllowedDomainList(), strictness),
			"drop or replace the link, or add the domain to allow_domains after review"
	default:
		return fmt.Sprintf("%s %s (strictness %s)", v.Code, v.Name, strictness), "re-publish with metadata the policy accepts"
	}
}

// refuses is why the policy would refuse a write of meta at path, as the
// store would (block and ask both reject); "" when it lands.
func (a *audit) refuses(p string, meta map[string]string, body string) string {
	if a.policy.EffectiveStrictness() == publishpolicy.Warn {
		return ""
	}
	var reasons []string
	for _, v := range a.violations(p, meta, body) {
		detail, _ := a.violation(v, meta)
		reasons = append(reasons, detail)
	}
	return strings.Join(reasons, "; ")
}

// checkContent scans every body for secrets, personal data and text that
// steers an agent. A reviewed override is reported with its reason, not a
// fix. Email stays unchecked: the doctor does not know the work domains.
func (a *audit) checkContent() {
	scanner := contentcheck.New(contentcheck.Options{PII: contentcheck.Block, Steering: contentcheck.Block})
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		findings := scanner.Scan(d.body)
		if len(findings) == 0 {
			continue
		}
		injected, sensitive := contentcheck.SplitSteering(findings)
		override := strings.TrimSpace(d.meta[contentcheck.OverrideKey])
		a.addContent(CheckInjection, p, injected, override)
		a.addContent(CheckContent, p, sensitive, override)
	}
}

// addContent reports one check's findings on a document; an override covers
// personal data and injection, never a secret.
func (a *audit) addContent(check, p string, findings []contentcheck.Finding, override string) {
	switch {
	case len(findings) == 0:
	case override != "" && !contentcheck.Secrets(findings):
		a.add(check, p, withOverride(contentcheck.Summary(findings), override), "")
	default:
		a.add(check, p, contentcheck.Summary(findings), contentcheck.StoredRemedy(findings))
	}
}

// checkHiddenText finds stored bodies and metadata values carrying text the
// write gate now refuses; versions predating the gate still serve it.
func (a *audit) checkHiddenText() {
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		if hits := hiddentext.Find(d.body); len(hits) > 0 {
			a.add(CheckHiddenText, p, hiddentext.Summary(hits), hiddenRemedy)
		}
		for _, k := range slices.Sorted(maps.Keys(d.meta)) {
			if hits := hiddentext.Find(d.meta[k]); len(hits) > 0 {
				a.add(CheckHiddenText, p, "metadata "+k+": "+hiddentext.Summary(hits), hiddenRemedy)
			}
		}
	}
}

// checkTainted lists documents stamped source-trust: tainted, the review
// queue for writes made after a session read untrusted content.
func (a *audit) checkTainted() {
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) || !protocol.IsTainted(d.meta) {
			continue
		}
		detail := protocol.TaintKey + ": " + protocol.TaintValue
		if from := strings.TrimSpace(d.meta[protocol.TaintFromKey]); from != "" {
			detail += " (" + hiddentext.Reveal(from) + ")"
		}
		a.add(CheckTainted, p, detail, "review the text against those reads; once reviewed, re-publish from a session that read nothing untrusted, without "+protocol.TaintKey)
	}
}

// checkClassifier lists versions the server's classifier flagged or could
// not check; an override reviews only a verdict, never a missing one.
func (a *audit) checkClassifier() {
	for _, p := range a.order {
		d := a.docs[p]
		verdict := d.meta[protocol.ClassifierKey]
		if !live(d.status) || verdict == "" {
			continue
		}
		detail := protocol.ClassifierKey + ": " + hiddentext.Reveal(verdict)
		override := strings.TrimSpace(d.meta[contentcheck.OverrideKey])
		switch {
		case strings.Contains(verdict, protocol.ClassifierOverLimit):
			a.add(CheckClassifier, p, detail, "shorten the body under the classifier's limit (or raise contentCheck.classifier.maxBytes), then re-publish so the version is checked")
		case !protocol.ClassifierFlagged(d.meta):
			a.add(CheckClassifier, p, detail, "re-publish once the classifier answers, so the version is checked")
		case override != "":
			a.add(CheckClassifier, p, withOverride(detail, override), "")
		default:
			a.add(CheckClassifier, p, detail, "review it against the reason; remove the text that steers an agent and re-publish, or for a false positive re-publish with metadata "+contentcheck.OverrideKey+" set to the reason")
		}
	}
}

// hiddenRemedy is the fix for a hidden-text finding: read surfaces show the
// span as <U+XXXX>, so a republish of what was read drops or keeps it visibly.
const hiddenRemedy = "re-publish without the hidden characters (reads show them as <U+XXXX>); review the text they hid"

// checkStyleGuide compares a published guide's rule table with this build's;
// fetched by path, since LIST hides `.well-known/` from the inventory. A
// world without a guide (every world but the floor's) has no finding.
func (a *audit) checkStyleGuide(ctx context.Context) {
	resp, err := a.store.Fetch(ctx, stylerules.DocumentPath)
	switch {
	case err != nil:
		a.note("fetch %s failed: %v; style guide check skipped", stylerules.DocumentPath, err)
		return
	case resp.Status == protocol.StatusNotFound, resp.Status == protocol.StatusArchived:
		return
	case resp.Status != protocol.StatusOK:
		a.note("fetch %s returned %s; style guide check skipped", stylerules.DocumentPath, resp.Status)
		return
	case stylerules.Current(resp.Body):
		return
	}
	detail := "the enforced-rules table differs from the gate's"
	if !strings.Contains(resp.Body, stylerules.BeginMarker) {
		detail = "no enforced-rules table between the stylerules markers"
	}
	a.add(CheckStyleGuide, stylerules.DocumentPath, detail, "re-publish with the current table between "+stylerules.BeginMarker+" and "+stylerules.EndMarker)
}
