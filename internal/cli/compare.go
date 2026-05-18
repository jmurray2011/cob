package cli

import (
	"context"

	"github.com/jmurray2011/cob/pkg/cob"
)

// assetCompare is one asset's manifest-vs-published comparison, shared by
// `verify` and `diff`. SrcSHA is empty when it can't be known without
// downloading (an S3 object published without a SHA-256 checksum).
type assetCompare struct {
	Name        string // stored asset name (the source filename)
	Key         string // manifest key ("" for published-only assets)
	Source      string // resolved source URI ("" for published-only)
	SrcSHA      string
	PubSHA      string
	InManifest  bool
	InPublished bool
	Err         error // source resolution failed
}

// compareManifestToPublished resolves each manifest source's SHA-256 (no
// asset download — S3 HeadObject / ca:// listing / local hash only) and
// matches it against the published version's assets by stored name.
func compareManifestToPublished(ctx context.Context, sources []NamedSource, reg *cob.Registry, coords *cob.PackageCoordinates) ([]assetCompare, error) {
	pub, err := reg.ListAssets(ctx, coords)
	if err != nil {
		return nil, err
	}
	pubByName := make(map[string]cob.AssetSummary, len(pub))
	for _, a := range pub {
		pubByName[a.Name] = a
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
		c.SrcSHA = meta.SHA256
		if pa, ok := pubByName[name]; ok {
			c.InPublished = true
			c.PubSHA = pa.SHA256
		}
		cmps = append(cmps, c)
	}

	// Published assets with no corresponding manifest source.
	for _, a := range pub {
		if !seen[a.Name] {
			cmps = append(cmps, assetCompare{Name: a.Name, PubSHA: a.SHA256, InPublished: true})
		}
	}
	return cmps, nil
}
