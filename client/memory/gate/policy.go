package gate

import (
	"maps"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/provenance"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// Missing words the policy's violations on the metadata and the body's links,
// each a whole finding, as the server would at commit: after its type
// default, and never asking for identity keys, which are the surface's.
func Missing(path string, meta map[string]string, body string, policy *publishpolicy.Policy) []string {
	if policy == nil {
		return nil
	}
	var out []string
	for _, v := range publishpolicy.EvaluateBody(policy.ForCaller(), path, storefmt.ApplyOKFTypeDefault(path, meta), body).Violations {
		switch v.Code {
		case publishpolicy.MissingTagAxis:
			out = append(out, "missing tag axis "+v.Name+" (an "+v.Name+":<value> tag)")
		case publishpolicy.MissingField:
			out = append(out, "missing metadata "+v.Name)
		case publishpolicy.DisallowedTagValue:
			out = append(out, "disallowed tag "+v.Name+" (allowed "+v.Axis+" values: "+policy.AllowedList(v.Axis)+")")
		case publishpolicy.UnpinnedSource:
			out = append(out, "unpinned "+provenance.Key+" value "+v.Name+" (pin "+provenance.PinForms+")")
		case publishpolicy.DisallowedLinkDomain:
			out = append(out, "link to "+v.Name+" outside allow_domains ("+policy.AllowedDomainList()+")")
		default:
			out = append(out, string(v.Code))
		}
	}
	return out
}

// Lift moves a frontmatter fence's keys into meta (a key the document already
// carries wins; name is the title; reserved, identity and server-stamped keys
// never) and names them.
func Lift(kv, meta map[string]string) []string {
	var keys []string
	for _, k := range slices.Sorted(maps.Keys(kv)) {
		v := strings.TrimSpace(kv[k])
		k = strings.ToLower(k)
		if k == "name" {
			k = "title"
		}
		if _, taken := meta[k]; v == "" || taken || protocol.IsReservedMetadataKey(k) || protocol.IsIdentityMetadataKey(k) || protocol.IsServerStampedKey(k) {
			continue
		}
		meta[k] = v
		keys = append(keys, k)
	}
	return keys
}
