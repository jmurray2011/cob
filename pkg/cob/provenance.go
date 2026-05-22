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
// bounded rather than unbounded.
const maxProvenanceBytes = 64 << 20 // 64 MiB

// Upstream status values for a ca:// origin.
const (
	UpstreamEmbedded = "embedded"          // upstream cob-provenance.json inlined
	UpstreamNoProv   = "no-cob-provenance" // upstream exists but not cob-published
	UpstreamMissing  = "missing"           // recorded coords no longer resolve (re-check time)
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

// Marshal renders the document as indented JSON, stamping the schema.
func (p *Provenance) Marshal() []byte {
	p.Schema = ProvenanceSchema
	b, _ := json.MarshalIndent(p, "", "  ")
	return append(b, '\n')
}

// NowStamp is the RFC3339 UTC timestamp used for chain events.
func NowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

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
