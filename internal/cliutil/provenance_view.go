package cliutil

import (
	"context"
	"strings"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/concurrency"
	"github.com/jmurray2011/cob/internal/output"
)

// PromotionStatusConcurrency bounds the parallel per-repo VersionStatus
// calls done by ls's promotion-status table, rm's downstream-copies
// probe, log's --check-references, and diff's --check-references. One
// constant, one budget — every "fan out 1 VersionStatus per unique
// repo" caller agrees on the same throttle.
const PromotionStatusConcurrency = 8

// ChainRefStatus is the result of probing whether a chain event's
// referenced repository still holds this version. The zero value means
// "not checked" — RenderChain treats a nil map and the zero value
// identically, so an unannotated render is the default.
type ChainRefStatus int

const (
	ChainRefUnchecked ChainRefStatus = iota // zero value — nothing rendered
	ChainRefOK                              // version still present
	ChainRefMissing                         // probe returned "not found"
	ChainRefUnknown                         // probe errored (auth/network)
)

// ProbeChainReferences fires a VersionStatus per unique repository in the
// chain (publish.Repository, promote.From, promote.To), in parallel, and
// returns each reference's status. Empty references and malformed
// "domain/repo" pairs are recorded as ChainRefUnknown rather than
// silently dropped, so the renderer can surface them. The package and
// version are taken from coords — chains travel with one version of one
// package, so the same triple is correct for every probe. Shared by
// `cob log --check-references` and `cob diff --check-references` so the
// two surfaces report the same way.
func ProbeChainReferences(ctx context.Context, registry *cob.Registry, coords *cob.PackageCoordinates, prov *cob.Provenance) map[string]ChainRefStatus {
	refs := make(map[string]struct{})
	for _, e := range prov.Chain {
		for _, r := range []string{e.Repository, e.From, e.To} {
			if r != "" {
				refs[r] = struct{}{}
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}

	// Convert the set to an ordered slice so ForEach can index-align;
	// rebuild the keyed map afterward. Cheaper than the mutex-around-map
	// pattern, and removes the lock altogether.
	refKeys := make([]string, 0, len(refs))
	for r := range refs {
		refKeys = append(refKeys, r)
	}
	probed := concurrency.ForEach(ctx, refKeys, PromotionStatusConcurrency, func(ctx context.Context, _ int, ref string) ChainRefStatus {
		domain, repo, ok := strings.Cut(ref, "/")
		if !ok || domain == "" || repo == "" {
			return ChainRefUnknown
		}
		probe := *coords
		probe.Domain = domain
		probe.Repository = repo
		_, exists, err := registry.VersionStatus(ctx, &probe)
		switch {
		case err != nil:
			return ChainRefUnknown
		case !exists:
			return ChainRefMissing
		default:
			return ChainRefOK
		}
	})
	statuses := make(map[string]ChainRefStatus, len(refKeys))
	for i, ref := range refKeys {
		statuses[ref] = probed[i]
	}
	return statuses
}

// ActorStr renders an Actor compactly, preferring the most identifying
// field that's present.
func ActorStr(a cob.Actor) string {
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

// RenderChain prints the append-only evidence log: who published/promoted
// the version, where, and when. No-op in JSON mode (out.Plain is silent).
// refs, if non-nil, annotates each repository reference (publish.Repository,
// promote.From/To) with whether the referenced version still exists — see
// ChainRefStatus. Pass nil to render unannotated (default for the diff
// self-check mode).
func RenderChain(out *output.Writer, p *cob.Provenance, refs map[string]ChainRefStatus) {
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
				e.Time, ActorStr(e.Actor))
			if e.ManifestSHA256 != "" {
				out.Plain("       manifest %s  cob %s  %s", Short(e.ManifestSHA256), e.CobVersion, e.Region)
			}
		case "promote":
			out.Plain("  %d. promote  %s%s → %s%s  %s  by %s",
				i+1, e.From, refTag(refs, e.From), e.To, refTag(refs, e.To),
				e.Time, ActorStr(e.Actor))
		default:
			out.Plain("  %d. %s  %s", i+1, e.Event, e.Time)
		}
	}
}

// refTag returns the inline annotation for a repository reference: nothing
// when the ref wasn't checked or still resolves, "(deleted)" when the
// referenced version is gone, "(?)" when the probe itself couldn't run.
// Designed to be silent on the common case so the chain stays readable.
func refTag(refs map[string]ChainRefStatus, ref string) string {
	if refs == nil || ref == "" {
		return ""
	}
	switch refs[ref] {
	case ChainRefMissing:
		return " (deleted)"
	case ChainRefUnknown:
		return " (?)"
	}
	return ""
}

// RenderOrigins prints where each file came from, recursing into embedded
// ca:// upstream provenance so the full trail is visible.
func RenderOrigins(out *output.Writer, p *cob.Provenance, indent string) {
	if indent == "" {
		out.Plain("where the files came from:")
	}
	for _, a := range p.Assets {
		out.Plain("%s  %s  (%s)  %s", indent, a.Asset, Short(a.SHA256), originStr(a.Origin))
		if a.Origin != nil && a.Origin.Type == "ca" && a.Origin.UpstreamProvenance != nil {
			up := a.Origin.UpstreamProvenance
			out.Plain("%s    ↳ upstream %s, published by %s", indent, up.Package, chainOrigin(up))
			RenderOrigins(out, up, indent+"    ")
		}
	}
}

// chainOrigin summarizes who first published an embedded upstream.
func chainOrigin(p *cob.Provenance) string {
	for _, e := range p.Chain {
		if e.Event == "publish" {
			return ActorStr(e.Actor)
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

// Short truncates a SHA-256 hex to a 12-char prefix for compact display.
// Same convention everywhere chain or asset hashes are rendered so the
// abbreviations match between cob log, cob diff, and cob manifest.
func Short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
