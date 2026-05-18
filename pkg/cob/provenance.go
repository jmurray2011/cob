package cob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
)

// ProvenanceFile is the well-known asset name cob writes into every
// published version. It records what each manifest source resolved to and
// the SHA-256 cob computed while streaming it, so verify/diff can work
// without re-reading S3 (and even when the S3 object has no checksum).
const ProvenanceFile = "cob-provenance.json"

// ProvenanceSchema is the current cob_provenance schema version.
const ProvenanceSchema = 1

// Provenance is the cob-provenance.json document.
type Provenance struct {
	Schema     int               `json:"cob_provenance"`
	Package    string            `json:"package"`
	Repository string            `json:"repository"`
	Version    string            `json:"version"`
	Published  string            `json:"published"`
	CobVersion string            `json:"cob_version,omitempty"`
	Assets     []ProvenanceEntry `json:"assets"`
}

// ProvenanceEntry is one manifest source as published.
type ProvenanceEntry struct {
	Key    string `json:"key"`    // manifest YAML key
	Source string `json:"source"` // resolved source URI
	Asset  string `json:"asset"`  // stored CodeArtifact asset name
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// SHAByAsset indexes the recorded SHA-256 by stored asset name.
func (p *Provenance) SHAByAsset() map[string]string {
	m := make(map[string]string, len(p.Assets))
	for _, a := range p.Assets {
		m[a.Asset] = a.SHA256
	}
	return m
}

// Marshal renders the provenance document as indented JSON.
func (p *Provenance) Marshal() []byte {
	p.Schema = ProvenanceSchema
	if p.Published == "" {
		p.Published = time.Now().UTC().Format(time.RFC3339)
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	return append(b, '\n')
}

// bytesSource is an in-memory AssetSource, used to publish the generated
// provenance document through the normal publish path.
type bytesSource struct {
	name string
	data []byte
}

// NewBytesSource creates an AssetSource backed by an in-memory buffer whose
// stored asset name is name.
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

// FetchProvenance reads and parses the cob-provenance.json asset for a
// version. It returns (nil, nil) when the version has no provenance asset
// (e.g. it was published by an older cob), so callers can fall back.
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

	data, err := io.ReadAll(out.Asset)
	if err != nil {
		return nil, err
	}
	var p Provenance
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
