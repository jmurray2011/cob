package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"

	"github.com/jmurray2011/cob/pkg/cob"
)

// assetCompare is one asset's manifest-vs-published comparison, shared by
// `verify` and `diff`.
//
// SrcSHA is the source's SHA-256 and SrcFrom records how it was obtained:
//
//	"source"     - from a known checksum (S3 SHA-256 / ca:// / local hash)
//	"deep"       - cob downloaded and hashed the source (--deep)
//	"provenance" - taken from the version's recorded cob-provenance.json
//	""           - unknown (no checksum, no --deep, no provenance)
type assetCompare struct {
	Name        string // stored asset name (the source filename)
	Key         string // manifest key ("" for published-only assets)
	Source      string // resolved source URI ("" for published-only)
	SrcSHA      string
	SrcFrom     string
	PubSHA      string
	InManifest  bool
	InPublished bool
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
	if prov != nil {
		provSHA = prov.SHAByAsset()
	}

	seen := make(map[string]bool, len(sources))
	var cmps []assetCompare

	for _, ns := range sources {
		name := ns.Source.Filename()
		seen[name] = true
		c := assetCompare{Name: name, Key: ns.Name, Source: ns.Source.URI(), InManifest: true}

		meta, rerr := ns.Source.Resolve(ctx)
		if rerr != nil {
			c.Err = rerr
			cmps = append(cmps, c)
			continue
		}
		switch {
		case meta.SHA256 != "":
			c.SrcSHA, c.SrcFrom = meta.SHA256, "source"
		case deep:
			h, herr := hashSource(ctx, ns.Source)
			if herr != nil {
				c.Err = herr
				cmps = append(cmps, c)
				continue
			}
			c.SrcSHA, c.SrcFrom = h, "deep"
		default:
			if s, ok := provSHA[name]; ok {
				c.SrcSHA, c.SrcFrom = s, "provenance"
			}
		}

		if pa, ok := pubByName[name]; ok {
			c.InPublished = true
			c.PubSHA = pa.SHA256
		}
		cmps = append(cmps, c)
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
