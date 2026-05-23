package cob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

// ProvenanceFile is the well-known asset cob writes into every published
// version: a chain-of-evidence document recording who published/promoted it
// and where each file came from, so verify/diff work without re-reading S3.
const ProvenanceFile = "cob-provenance.json"

// ProvenanceSchema is the current cob_provenance schema version.
const ProvenanceSchema = 2

// maxProvenanceBytes caps the provenance document cob will read. It grows
// with the dependency closure and is upstream-controlled, so the read is
// bounded rather than unbounded. The same ceiling applies on the write
// path (Marshal) so a downstream cob cannot publish a document its own
// readers would refuse.
const maxProvenanceBytes = 64 << 20 // 64 MiB

// maxUpstreamDepth caps how many levels of UpstreamProvenance embedding
// cob will preserve when building a new document. A direct ca:// source
// brings depth-1; that source's own upstreams ride along to a total of
// maxUpstreamDepth from the new root. Anything past that is dropped to
// UpstreamTruncated — the chain coords on each cut Origin still let a
// reader walk further via cob log if they want, so no history is lost,
// just inlined.
const maxUpstreamDepth = 8

// Upstream status values for a ca:// origin.
const (
	UpstreamEmbedded  = "embedded"          // upstream cob-provenance.json inlined
	UpstreamNoProv    = "no-cob-provenance" // upstream exists but not cob-published
	UpstreamMissing   = "missing"           // recorded coords no longer resolve (re-check time)
	UpstreamTruncated = "depth-truncated"   // chain cut to keep document bounded; re-fetch via cob log
)

// Provenance is the cob-provenance.json document. assets is invariant across
// promotes (the same bytes move); chain is an append-only evidence log.
type Provenance struct {
	Schema  int               `json:"cob_provenance"`
	Package string            `json:"package"`
	Assets  []ProvenanceEntry `json:"assets"`
	Chain   []ProvenanceEvent `json:"chain"`
}

// ProvenanceEntry is one manifest source as published.
type ProvenanceEntry struct {
	Key    string  `json:"key"`    // manifest YAML key
	Source string  `json:"source"` // resolved source URI
	Asset  string  `json:"asset"`  // stored CodeArtifact asset name
	SHA256 string  `json:"sha256"`
	Size   int64   `json:"size"`
	Origin *Origin `json:"origin,omitempty"` // where the file physically came from
}

// Actor is the AWS principal that performed a chain event (STS identity).
type Actor struct {
	Account string `json:"account,omitempty"`
	ARN     string `json:"arn,omitempty"`
	UserID  string `json:"user_id,omitempty"`
}

// ProvenanceEvent is one link in the chain: a publish or a promote.
type ProvenanceEvent struct {
	Event          string `json:"event"`                // "publish" | "promote"
	Repository     string `json:"repository,omitempty"` // publish: domain/repo
	Version        string `json:"version,omitempty"`    // publish
	From           string `json:"from,omitempty"`       // promote: domain/repo
	To             string `json:"to,omitempty"`         // promote: domain/repo
	Time           string `json:"time"`
	CobVersion     string `json:"cob_version,omitempty"`
	Region         string `json:"region,omitempty"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"` // publish only
	Actor          Actor  `json:"actor"`
}

// Origin pins where one asset physically came from at packaging time.
// Type is "s3", "ca", or "file"; only the matching fields are populated.
type Origin struct {
	Type string `json:"type"`

	// s3
	Bucket       string `json:"bucket,omitempty"`
	Key          string `json:"key,omitempty"`
	VersionID    string `json:"version_id,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Region       string `json:"region,omitempty"`
	Versioned    *bool  `json:"versioned,omitempty"`

	// ca (recursive: upstream's own provenance, frozen at our publish time)
	Domain             string      `json:"domain,omitempty"`
	CARepository       string      `json:"ca_repository,omitempty"`
	Namespace          string      `json:"namespace,omitempty"`
	Package            string      `json:"package,omitempty"`
	CAVersion          string      `json:"ca_version,omitempty"`
	CAAsset            string      `json:"ca_asset,omitempty"`
	UpstreamStatus     string      `json:"upstream_status,omitempty"`
	UpstreamProvenance *Provenance `json:"upstream_provenance,omitempty"`

	// file
	Path  string `json:"path,omitempty"`
	Mtime string `json:"mtime,omitempty"`
}

// Validate enforces the per-Type invariants on Origin. The flat JSON
// shape means each Origin populates only a subset of fields; this turns
// the "only the matching fields are populated" convention into an
// explicit check at the serialize boundary, so a corrupt origin
// (refactor bug, garbage from a malformed upstream document) surfaces
// loudly here instead of silently producing nonsense downstream.
//
// Called from Provenance.Marshal once per asset; never blocks reads,
// so a v1-era document with sloppy origins can still be loaded — only
// brand-new writes are gated. Recurses into embedded UpstreamProvenance
// so an Origin inside a ca chain that's been tampered with surfaces
// too. A nil Origin (the provenance finalizer asset itself uses this)
// is fine.
func (o *Origin) Validate() error {
	if o == nil {
		return nil
	}
	mustBeEmpty := func(kind string, fields ...struct{ name, val string }) error {
		for _, f := range fields {
			if f.val != "" {
				return fmt.Errorf("%s origin: %s must be empty (cross-type pollution), got %q", kind, f.name, f.val)
			}
		}
		return nil
	}
	switch o.Type {
	case "":
		// No type ⇒ origin not recorded. The publisher may legitimately
		// omit Origin entirely; the field is best-effort metadata.
		return nil
	case "s3":
		if o.Bucket == "" || o.Key == "" {
			return fmt.Errorf("s3 origin: bucket and key required, got bucket=%q key=%q", o.Bucket, o.Key)
		}
		if err := mustBeEmpty("s3",
			struct{ name, val string }{"domain", o.Domain},
			struct{ name, val string }{"ca_repository", o.CARepository},
			struct{ name, val string }{"namespace", o.Namespace},
			struct{ name, val string }{"package", o.Package},
			struct{ name, val string }{"ca_version", o.CAVersion},
			struct{ name, val string }{"ca_asset", o.CAAsset},
			struct{ name, val string }{"upstream_status", o.UpstreamStatus},
			struct{ name, val string }{"path", o.Path},
			struct{ name, val string }{"mtime", o.Mtime},
		); err != nil {
			return err
		}
		if o.UpstreamProvenance != nil {
			return fmt.Errorf("s3 origin: upstream_provenance must be empty (ca-only field)")
		}
	case "ca":
		if o.Domain == "" || o.CARepository == "" || o.Namespace == "" || o.Package == "" || o.CAVersion == "" || o.CAAsset == "" {
			return fmt.Errorf("ca origin: domain/ca_repository/namespace/package/ca_version/ca_asset all required, got domain=%q repo=%q ns=%q pkg=%q ver=%q asset=%q",
				o.Domain, o.CARepository, o.Namespace, o.Package, o.CAVersion, o.CAAsset)
		}
		if err := mustBeEmpty("ca",
			struct{ name, val string }{"bucket", o.Bucket},
			struct{ name, val string }{"key", o.Key},
			struct{ name, val string }{"version_id", o.VersionID},
			struct{ name, val string }{"etag", o.ETag},
			struct{ name, val string }{"last_modified", o.LastModified},
			struct{ name, val string }{"region", o.Region},
			struct{ name, val string }{"path", o.Path},
			struct{ name, val string }{"mtime", o.Mtime},
		); err != nil {
			return err
		}
		if o.Versioned != nil {
			return fmt.Errorf("ca origin: versioned must be empty (s3-only field)")
		}
		// Recurse into the embedded upstream so a tampered chain element
		// surfaces at the same write boundary.
		if o.UpstreamProvenance != nil {
			for i, a := range o.UpstreamProvenance.Assets {
				if err := a.Origin.Validate(); err != nil {
					return fmt.Errorf("upstream provenance asset[%d] (%s): %w", i, a.Asset, err)
				}
			}
		}
	case "file":
		if o.Path == "" {
			return fmt.Errorf("file origin: path required")
		}
		if err := mustBeEmpty("file",
			struct{ name, val string }{"bucket", o.Bucket},
			struct{ name, val string }{"key", o.Key},
			struct{ name, val string }{"version_id", o.VersionID},
			struct{ name, val string }{"etag", o.ETag},
			struct{ name, val string }{"last_modified", o.LastModified},
			struct{ name, val string }{"region", o.Region},
			struct{ name, val string }{"domain", o.Domain},
			struct{ name, val string }{"ca_repository", o.CARepository},
			struct{ name, val string }{"namespace", o.Namespace},
			struct{ name, val string }{"package", o.Package},
			struct{ name, val string }{"ca_version", o.CAVersion},
			struct{ name, val string }{"ca_asset", o.CAAsset},
			struct{ name, val string }{"upstream_status", o.UpstreamStatus},
		); err != nil {
			return err
		}
		if o.Versioned != nil {
			return fmt.Errorf("file origin: versioned must be empty (s3-only field)")
		}
		if o.UpstreamProvenance != nil {
			return fmt.Errorf("file origin: upstream_provenance must be empty (ca-only field)")
		}
	default:
		return fmt.Errorf("unknown origin type %q (expected s3, ca, or file)", o.Type)
	}
	return nil
}

// SHAByAsset indexes the recorded SHA-256 by stored asset name. Field name
// is stable across schema versions, so this also works on v1 documents.
func (p *Provenance) SHAByAsset() map[string]string {
	m := make(map[string]string, len(p.Assets))
	for _, a := range p.Assets {
		m[a.Asset] = a.SHA256
	}
	return m
}

// OriginByAsset indexes the recorded Origin by stored asset name.
func (p *Provenance) OriginByAsset() map[string]*Origin {
	m := make(map[string]*Origin, len(p.Assets))
	for _, a := range p.Assets {
		if a.Origin != nil {
			m[a.Asset] = a.Origin
		}
	}
	return m
}

// Marshal renders the document as indented JSON with the current schema
// stamped. It does not mutate the receiver. Two failure modes the caller
// must handle: the encoder rejects the value (in practice unreachable for
// the fixed struct shape, but reachable in principle once Origin carries
// arbitrary upstream-supplied content), or the resulting document exceeds
// maxProvenanceBytes — typically the depth-cap escape hatch wasn't enough
// for a fan-out-heavy upstream. PruneUpstreamProvenance gives callers a
// way to retry with the upstream tree dropped.
func (p *Provenance) Marshal() ([]byte, error) {
	// Per-asset Origin invariants — caught here so a corrupt origin
	// (refactor bug, malformed upstream document) fails the write
	// instead of producing a doc that downstream readers can't trust.
	for i, a := range p.Assets {
		if err := a.Origin.Validate(); err != nil {
			return nil, fmt.Errorf("asset[%d] (%s): %w", i, a.Asset, err)
		}
	}
	doc := *p
	doc.Schema = ProvenanceSchema
	b, err := json.MarshalIndent(&doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling provenance: %w", err)
	}
	b = append(b, '\n')
	if int64(len(b)) > maxProvenanceBytes {
		return nil, fmt.Errorf("provenance document is %d bytes, exceeds the %d-byte limit", len(b), maxProvenanceBytes)
	}
	return b, nil
}

// PruneUpstreamProvenance walks p in place and limits how deeply
// UpstreamProvenance nests below it. remaining is the number of further
// embedding levels permitted; once 0, any ca-origin asset with an inlined
// upstream is rewritten to UpstreamTruncated with the upstream document
// dropped. Coordinates on the Origin survive — a reader can still walk
// the chain via cob log if they need depth past what's inlined.
//
// Called by CASource.Origin on every fetched upstream so the document a
// downstream publishes can never grow without bound, and by the publish
// finalize fallback when marshaling still trips the size cap.
func PruneUpstreamProvenance(p *Provenance, remaining int) {
	if p == nil {
		return
	}
	for i := range p.Assets {
		o := p.Assets[i].Origin
		if o == nil || o.UpstreamProvenance == nil {
			continue
		}
		if remaining <= 0 {
			o.UpstreamProvenance = nil
			o.UpstreamStatus = UpstreamTruncated
			continue
		}
		PruneUpstreamProvenance(o.UpstreamProvenance, remaining-1)
	}
}

// now is the time source for provenance stamps. It is a package var so a
// test can pin it and compare generated provenance against a golden value.
var now = time.Now

// NowStamp is the RFC3339 UTC timestamp used for chain events.
func NowStamp() string { return now().UTC().Format(time.RFC3339) }

// bytesSource is an in-memory AssetSource used to publish the generated
// provenance document through the normal publish path.
type bytesSource struct {
	name string
	data []byte
}

// NewBytesSource creates an AssetSource backed by an in-memory buffer.
func NewBytesSource(name string, data []byte) AssetSource {
	return &bytesSource{name: name, data: data}
}

func (b *bytesSource) URI() string      { return "cob://" + b.name }
func (b *bytesSource) Filename() string { return b.name }

func (b *bytesSource) Resolve(context.Context) (*AssetMetadata, error) {
	sum := sha256.Sum256(b.data)
	return &AssetMetadata{Size: int64(len(b.data)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func (b *bytesSource) Open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.data)), nil
}

// Origin is nil for the provenance asset itself — it is cob's own output,
// never a manifest source whose origin we record.
func (b *bytesSource) Origin(context.Context) (*Origin, error) { return nil, nil }

// FetchProvenance reads and parses the cob-provenance.json asset for a
// version. Returns (nil, nil) when the version has no provenance asset (an
// older cob or a non-cob publisher), so callers can fall back.
func FetchProvenance(ctx context.Context, ca CodeArtifactAPI, coords *PackageCoordinates) (*Provenance, error) {
	out, err := ca.GetPackageVersionAsset(ctx, &codeartifact.GetPackageVersionAssetInput{
		Domain:         aws.String(coords.Domain),
		Repository:     aws.String(coords.Repository),
		Namespace:      aws.String(coords.Namespace),
		Package:        aws.String(coords.Package),
		PackageVersion: aws.String(coords.Version),
		Format:         FormatGeneric,
		Asset:          aws.String(ProvenanceFile),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer out.Asset.Close()

	data, err := io.ReadAll(io.LimitReader(out.Asset, maxProvenanceBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxProvenanceBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", ProvenanceFile, maxProvenanceBytes)
	}
	var p Provenance
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
