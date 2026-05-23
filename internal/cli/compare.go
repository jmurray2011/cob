package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/concurrency"
)

// compareConcurrency bounds parallel source resolution in
// compareManifestToPublished. Each source costs 1–3 network calls
// (Resolve, optional Origin head, optional --deep download); a 50-source
// manifest was minutes of sequential work pre-fanout, well into "diff
// too slow to use as a CI gate" territory.
const compareConcurrency = 8

// assetCompare is one asset's manifest-vs-published comparison, shared by
// `verify` and `diff`.
//
// SrcSHA is the source's SHA-256 and SrcFrom records how it was obtained:
//
//	"source"     - from a known checksum (S3 SHA-256 / ca:// / local hash)
//	"deep"       - cob downloaded and hashed the source (--deep)
//	"origin"     - the recorded S3 origin (etag/version_id) still matches, so
//	               the provenance SHA is trusted (cheap, no download)
//	"provenance" - taken from the version's recorded cob-provenance.json
//	""           - unknown (no checksum, no --deep, no provenance)
//
// OriginDrift is set when the recorded S3 origin no longer matches the live
// object (etag/version_id changed, or it's gone) — a no-download drift
// signal; treated as a mismatch by verify and a change by diff.
type assetCompare struct {
	Name        string // stored asset name (the source filename)
	Key         string // manifest key ("" for published-only assets)
	Source      string // resolved source URI ("" for published-only)
	SrcSHA      string
	SrcFrom     string
	PubSHA      string
	InManifest  bool
	InPublished bool
	OriginDrift bool
	Err         error // source resolution / deep-hash failed
}

// compareManifestToPublished resolves each manifest source's SHA-256 and
// matches it against the published version's assets by stored name.
//
// SHA precedence: a known checksum (no transfer) → a --deep download+hash →
// the recorded provenance value. The provenance asset itself is never
// treated as a manifest source or an "extra" published asset.
func compareManifestToPublished(ctx context.Context, sources []NamedSource, reg *cob.Registry, coords *cob.PackageCoordinates, deep bool, prov *cob.Provenance) ([]assetCompare, error) {
	pub, err := reg.ListAssets(ctx, coords)
	if err != nil {
		return nil, err
	}
	pubByName := make(map[string]cob.AssetSummary, len(pub))
	for _, a := range pub {
		pubByName[a.Name] = a
	}
	var provSHA map[string]string
	var provOrigin map[string]*cob.Origin
	if prov != nil {
		provSHA = prov.SHAByAsset()
		provOrigin = prov.OriginByAsset()
	}

	// Resolve every source in parallel: each iteration is independent and
	// makes 1–3 network calls. ForEach preserves input order, so the
	// resulting slice keeps the manifest's source ordering (important for
	// stable diff output across re-runs).
	cmps := concurrency.ForEach(ctx, sources, compareConcurrency, func(ctx context.Context, _ int, ns NamedSource) assetCompare {
		name := ns.Source.Filename()
		c := assetCompare{Name: name, Key: ns.Name, Source: ns.Source.URI(), InManifest: true}

		meta, rerr := ns.Source.Resolve(ctx)
		if rerr != nil {
			c.Err = rerr
			return c
		}
		switch {
		case meta.SHA256 != "":
			c.SrcSHA, c.SrcFrom = meta.SHA256, "source"
		case deep:
			h, herr := hashSource(ctx, ns.Source)
			if herr != nil {
				c.Err = herr
				return c
			}
			c.SrcSHA, c.SrcFrom = h, "deep"
		default:
			// No content checksum and no --deep. If provenance recorded an
			// S3 origin, do a cheap no-download drift check: HeadObject now
			// and compare etag/version_id to what was recorded.
			if rec := provOrigin[name]; rec != nil && rec.Type == "s3" {
				cur, oerr := ns.Source.Origin(ctx)
				if oerr != nil || originS3Changed(rec, cur) {
					c.OriginDrift, c.SrcFrom = true, "origin"
				} else if s, ok := provSHA[name]; ok {
					c.SrcSHA, c.SrcFrom = s, "origin"
				}
			} else if s, ok := provSHA[name]; ok {
				c.SrcSHA, c.SrcFrom = s, "provenance"
			}
		}

		if pa, ok := pubByName[name]; ok {
			c.InPublished = true
			c.PubSHA = pa.SHA256
		}
		return c
	})

	// Index manifest-derived rows by stored asset name so the
	// published-only pass can skip them in O(1).
	seen := make(map[string]bool, len(cmps))
	for _, c := range cmps {
		seen[c.Name] = true
	}
	// Published assets with no manifest source. The provenance asset is
	// cob's own and is never a manifest source, so don't report it.
	for _, a := range pub {
		if a.Name == cob.ProvenanceFile || seen[a.Name] {
			continue
		}
		cmps = append(cmps, assetCompare{Name: a.Name, PubSHA: a.SHA256, InPublished: true})
	}
	return cmps, nil
}

// originS3Changed reports whether a live S3 origin differs from the one
// recorded in provenance. Prefer version_id (immutable) when the recorded
// object was versioned; otherwise compare etag. If neither side has a
// comparable value, report unchanged rather than a false positive.
func originS3Changed(rec, cur *cob.Origin) bool {
	if cur == nil {
		return true // couldn't read it now ⇒ treat as drift
	}
	if rec.Versioned != nil && *rec.Versioned && rec.VersionID != "" {
		return cur.VersionID != rec.VersionID
	}
	if rec.ETag != "" && cur.ETag != "" {
		return cur.ETag != rec.ETag
	}
	return false
}

// hashSource streams a source and returns its hex SHA-256. Used by --deep
// to verify S3 objects that were uploaded without a SHA-256 checksum.
func hashSource(ctx context.Context, src cob.AssetSource) (string, error) {
	r, err := src.Open(ctx)
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
