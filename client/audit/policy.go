package audit

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/contentcheck"
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
	a.policy = own
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
	policy := a.policy.ForCaller()
	strictness := a.policy.EffectiveStrictness()
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		for _, v := range publishpolicy.EvaluateStrings(policy, p, d.meta).Violations {
			switch v.Code {
			case publishpolicy.MissingTags:
				continue
			case publishpolicy.InvalidImportance:
				a.add(CheckPolicy, p, fmt.Sprintf("importance %q is not a number in [0,1] (strictness %s)", d.meta["importance"], strictness), "re-publish with a valid metadata.importance")
			case publishpolicy.MissingTagAxis:
				a.add(CheckPolicy, p, fmt.Sprintf("missing required tag axis %s: (strictness %s)", v.Name, strictness), "re-publish with a "+v.Name+":<value> tag")
			case publishpolicy.MissingField:
				a.add(CheckPolicy, p, fmt.Sprintf("missing required metadata %s (strictness %s)", v.Name, strictness), "re-publish with metadata."+v.Name)
			case publishpolicy.DisallowedTagValue:
				a.add(CheckPolicy, p, fmt.Sprintf("tag %s is outside the allowed values for %s (%s) (strictness %s)",
					v.Name, v.Axis, policy.AllowedList(v.Axis), strictness), "re-publish with an allowed "+v.Axis+": value")
			default:
				a.add(CheckPolicy, p, fmt.Sprintf("%s %s (strictness %s)", v.Code, v.Name, strictness), "re-publish with metadata the policy accepts")
			}
		}
	}
}

// checkContent scans every body for secrets and personal data. An override
// is reported with its reason, not a fix, so overrides can be reviewed.
// Email stays unchecked: the doctor does not know the work domains.
func (a *audit) checkContent() {
	scanner := contentcheck.New(contentcheck.Options{PII: contentcheck.Block})
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		findings := scanner.Scan(d.body)
		if len(findings) == 0 {
			continue
		}
		if override := strings.TrimSpace(d.meta[contentcheck.OverrideKey]); override != "" {
			a.add(CheckContent, p, contentcheck.Summary(findings)+" (override: "+override+")", "")
			continue
		}
		a.add(CheckContent, p, contentcheck.Summary(findings), "remove it and re-publish, or for a false positive re-publish with metadata "+contentcheck.OverrideKey+" set to the reason")
	}
}

// checkStyleGuide compares a published style guide's rule table with the
// one this build renders; a guide without the table is stale too.
func (a *audit) checkStyleGuide() {
	d, ok := a.docs[stylerules.DocumentPath]
	if !ok || !live(d.status) || stylerules.Current(d.body) {
		return
	}
	detail := "the enforced-rules table differs from the gate's"
	if !strings.Contains(d.body, stylerules.BeginMarker) {
		detail = "no enforced-rules table between the stylerules markers"
	}
	a.add(CheckStyleGuide, stylerules.DocumentPath, detail, "re-publish with the current table between "+stylerules.BeginMarker+" and "+stylerules.EndMarker)
}
