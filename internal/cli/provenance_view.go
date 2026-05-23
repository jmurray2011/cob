package cli

import (
	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

// chainRefStatus is the result of probing whether a chain event's
// referenced repository still holds this version. The zero value means
// "not checked" — renderChain treats a nil map and the zero value
// identically, so an unannotated render is the default.
type chainRefStatus int

const (
	chainRefUnchecked chainRefStatus = iota // zero value — nothing rendered
	chainRefOK                              // version still present
	chainRefMissing                         // probe returned "not found"
	chainRefUnknown                         // probe errored (auth/network)
)

// actorStr renders an Actor compactly, preferring the most identifying
// field that's present.
func actorStr(a cob.Actor) string {
	switch {
	case a.ARN != "":
		return a.ARN
	case a.UserID != "":
		return a.UserID
	case a.Account != "":
		return "account " + a.Account
	default:
		return "unknown"
	}
}

// renderChain prints the append-only evidence log: who published/promoted
// the version, where, and when. No-op in JSON mode (out.Plain is silent).
// refs, if non-nil, annotates each repository reference (publish.Repository,
// promote.From/To) with whether the referenced version still exists — see
// chainRefStatus. Pass nil to render unannotated (verify mode).
func renderChain(out *output.Writer, p *cob.Provenance, refs map[string]chainRefStatus) {
	out.Plain("chain of evidence:")
	if len(p.Chain) == 0 {
		out.Plain("  (none recorded)")
		return
	}
	for i, e := range p.Chain {
		switch e.Event {
		case "publish":
			out.Plain("  %d. publish  %s@%s%s  %s  by %s",
				i+1, e.Repository, e.Version, refTag(refs, e.Repository),
				e.Time, actorStr(e.Actor))
			if e.ManifestSHA256 != "" {
				out.Plain("       manifest %s  cob %s  %s", short(e.ManifestSHA256), e.CobVersion, e.Region)
			}
		case "promote":
			out.Plain("  %d. promote  %s%s → %s%s  %s  by %s",
				i+1, e.From, refTag(refs, e.From), e.To, refTag(refs, e.To),
				e.Time, actorStr(e.Actor))
		default:
			out.Plain("  %d. %s  %s", i+1, e.Event, e.Time)
		}
	}
}

// refTag returns the inline annotation for a repository reference: nothing
// when the ref wasn't checked or still resolves, "(deleted)" when the
// referenced version is gone, "(?)" when the probe itself couldn't run.
// Designed to be silent on the common case so the chain stays readable.
func refTag(refs map[string]chainRefStatus, ref string) string {
	if refs == nil || ref == "" {
		return ""
	}
	switch refs[ref] {
	case chainRefMissing:
		return " (deleted)"
	case chainRefUnknown:
		return " (?)"
	}
	return ""
}

// renderOrigins prints where each file came from, recursing into embedded
// ca:// upstream provenance so the full trail is visible.
func renderOrigins(out *output.Writer, p *cob.Provenance, indent string) {
	if indent == "" {
		out.Plain("where the files came from:")
	}
	for _, a := range p.Assets {
		out.Plain("%s  %s  (%s)  %s", indent, a.Asset, short(a.SHA256), originStr(a.Origin))
		if a.Origin != nil && a.Origin.Type == "ca" && a.Origin.UpstreamProvenance != nil {
			up := a.Origin.UpstreamProvenance
			out.Plain("%s    ↳ upstream %s, published by %s", indent, up.Package, chainOrigin(up))
			renderOrigins(out, up, indent+"    ")
		}
	}
}

// chainOrigin summarizes who first published an embedded upstream.
func chainOrigin(p *cob.Provenance) string {
	for _, e := range p.Chain {
		if e.Event == "publish" {
			return actorStr(e.Actor)
		}
	}
	return "unknown"
}

func originStr(o *cob.Origin) string {
	if o == nil {
		return "origin: (unrecorded)"
	}
	switch o.Type {
	case "s3":
		s := "s3://" + o.Bucket + "/" + o.Key
		if o.VersionID != "" {
			s += " v=" + o.VersionID
		} else if o.ETag != "" {
			s += " etag=" + o.ETag
		}
		return s
	case "ca":
		return "ca://" + o.Domain + "/" + o.CARepository + "/" + o.Namespace + "/" +
			o.Package + "@" + o.CAVersion + "/" + o.CAAsset + " [" + o.UpstreamStatus + "]"
	case "file":
		return "file " + o.Path + " (" + o.Mtime + ")"
	default:
		return "origin: " + o.Type
	}
}
