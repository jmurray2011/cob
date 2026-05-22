package cli

import (
	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
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
func renderChain(out *output.Writer, p *cob.Provenance) {
	out.Plain("chain of evidence:")
	if len(p.Chain) == 0 {
		out.Plain("  (none recorded)")
		return
	}
	for i, e := range p.Chain {
		switch e.Event {
		case "publish":
			out.Plain("  %d. publish  %s@%s  %s  by %s", i+1, e.Repository, e.Version, e.Time, actorStr(e.Actor))
			if e.ManifestSHA256 != "" {
				out.Plain("       manifest %s  cob %s  %s", short(e.ManifestSHA256), e.CobVersion, e.Region)
			}
		case "promote":
			out.Plain("  %d. promote  %s → %s  %s  by %s", i+1, e.From, e.To, e.Time, actorStr(e.Actor))
		default:
			out.Plain("  %d. %s  %s", i+1, e.Event, e.Time)
		}
	}
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
