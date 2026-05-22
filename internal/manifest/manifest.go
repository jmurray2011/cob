package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SourceEntry is an asset name + source URI pair, preserving manifest order.
type SourceEntry struct {
	Name string
	URI  string
}

// Manifest represents a parsed cob manifest file. It is built from a
// manifestDoc by Load; it is not itself a YAML decode target, so it carries
// no yaml tags.
type Manifest struct {
	Domain     string
	Repository string
	Namespace  string
	Package    string
	Promote    *PromoteConfig

	// Sources preserves the order from the YAML file.
	Sources []SourceEntry

	// Dir is the directory containing the manifest file.
	// Used for resolving relative paths in sources.
	Dir string

	// Overrides records which manifest fields were overridden by env vars.
	Overrides []EnvOverride

	// Raw is the exact byte content Load decoded, kept so SHA256 can digest
	// the manifest that actually produced a publish without re-reading it.
	Raw []byte
}

// SHA256 returns the hex SHA-256 of the manifest's raw bytes, or "" if it was
// not built by Load. Stamped into provenance — digesting the bytes already in
// hand (rather than re-reading the file) closes a TOCTOU gap for a field
// whose whole purpose is integrity.
func (m *Manifest) SHA256() string {
	if m.Raw == nil {
		return ""
	}
	sum := sha256.Sum256(m.Raw)
	return hex.EncodeToString(sum[:])
}

// PromoteConfig holds the promotion stage list.
type PromoteConfig struct {
	Stages []string `yaml:"stages"`
}

// manifestDoc is the on-disk shape of a manifest and the sole YAML decode
// target. Every recognised key is a struct field, so decoding with
// KnownFields(true) turns a typo (repositroy:, promtoe:) into a hard error
// instead of a silently dropped field. `sources` is decoded as a raw node so
// its key order survives — a Go map would lose it.
type manifestDoc struct {
	Domain     string         `yaml:"domain"`
	Repository string         `yaml:"repository"`
	Namespace  string         `yaml:"namespace"`
	Package    string         `yaml:"package"`
	Promote    *PromoteConfig `yaml:"promote,omitempty"`
	Sources    yaml.Node      `yaml:"sources"`
}

// Load reads and parses a manifest file from disk.
// Source key order from the YAML is preserved in Sources.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	var doc manifestDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("invalid manifest %s: file is empty", path)
		}
		return nil, fmt.Errorf("invalid manifest %s: %w", path, err)
	}

	sources, err := sourcesFromNode(&doc.Sources)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	m := &Manifest{
		Domain:     doc.Domain,
		Repository: doc.Repository,
		Namespace:  doc.Namespace,
		Package:    doc.Package,
		Promote:    doc.Promote,
		Sources:    sources,
		Dir:        filepath.Dir(abs),
		Raw:        data,
	}
	m.Overrides = m.applyEnvOverrides()

	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest %s: %w", path, err)
	}
	return m, nil
}

// EnvOverride records a single manifest field overridden by an env var.
type EnvOverride struct {
	Field string // e.g. "domain"
	Env   string // e.g. "COB_DOMAIN"
	Value string // the env var's value
}

// applyEnvOverrides applies COB_* environment variable overrides.
// Returns a record of each override applied so callers can log them.
func (m *Manifest) applyEnvOverrides() []EnvOverride {
	var overrides []EnvOverride

	apply := func(field, env string, target *string) {
		if v := os.Getenv(env); v != "" {
			*target = v
			overrides = append(overrides, EnvOverride{Field: field, Env: env, Value: v})
		}
	}

	apply("domain", "COB_DOMAIN", &m.Domain)
	apply("repository", "COB_REPOSITORY", &m.Repository)
	apply("namespace", "COB_NAMESPACE", &m.Namespace)
	apply("package", "COB_PACKAGE", &m.Package)

	return overrides
}

// sourcesFromNode converts the raw `sources:` mapping node into ordered
// SourceEntry values. A zero node (the key was absent) yields nil, which
// validate() then rejects with a clearer "at least one source" message.
func sourcesFromNode(node *yaml.Node) ([]SourceEntry, error) {
	if node == nil || node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("sources must be a mapping")
	}
	entries := make([]SourceEntry, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if val.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("source %q must be a string URI", key.Value)
		}
		entries = append(entries, SourceEntry{Name: key.Value, URI: val.Value})
	}
	return entries, nil
}

func (m *Manifest) validate() error {
	if m.Domain == "" {
		return fmt.Errorf("domain is required")
	}
	if m.Repository == "" {
		return fmt.Errorf("repository is required")
	}
	if m.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if m.Package == "" {
		return fmt.Errorf("package is required")
	}
	if len(m.Sources) == 0 {
		return fmt.Errorf("at least one source is required")
	}
	for _, s := range m.Sources {
		if s.Name == "" {
			return fmt.Errorf("empty asset name")
		}
		if s.URI == "" {
			return fmt.Errorf("empty source URI for asset %q", s.Name)
		}
	}
	return nil
}

// varPattern matches ${VERSION} and ${env.WHATEVER}.
var varPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// ResolveVariables expands ${VERSION} and ${env.*} in all source URIs.
// Returns an error if any variable is unresolved.
func (m *Manifest) ResolveVariables(version string) error {
	for i, s := range m.Sources {
		resolved, err := expandVars(s.URI, version)
		if err != nil {
			return fmt.Errorf("asset %q: %w", s.Name, err)
		}
		m.Sources[i].URI = resolved
	}
	return nil
}

// ExpandURI resolves ${VERSION} and ${env.*} in one source URI. Exposed for
// offline validation, which checks sources individually and reports every
// problem rather than aborting at the first bad one (unlike ResolveVariables).
func ExpandURI(uri, version string) (string, error) {
	return expandVars(uri, version)
}

func expandVars(s, version string) (string, error) {
	var expandErr error
	result := varPattern.ReplaceAllStringFunc(s, func(match string) string {
		// Extract the variable name from ${...}
		varName := match[2 : len(match)-1]

		switch {
		case varName == "VERSION":
			if version == "" {
				expandErr = fmt.Errorf("${VERSION} used but no version provided (use --version or COB_VERSION)")
				return match
			}
			return version
		case strings.HasPrefix(varName, "env."):
			// ${env.X} reads COB_VAR_X — not arbitrary environment. This
			// namespacing keeps a manifest from pulling a secret like
			// AWS_SECRET_ACCESS_KEY into a source URI (which would be
			// recorded in published provenance / sent to the URI's host).
			name := varName[4:]
			if name == "" {
				expandErr = fmt.Errorf("${env.} has an empty variable name")
				return match
			}
			envKey := "COB_VAR_" + name
			val, ok := os.LookupEnv(envKey)
			if !ok {
				expandErr = fmt.Errorf("${%s} references unset environment variable %s", varName, envKey)
				return match
			}
			return val
		default:
			expandErr = fmt.Errorf("unknown variable ${%s} (use ${VERSION} or ${env.NAME})", varName)
			return match
		}
	})

	if expandErr != nil {
		return "", expandErr
	}
	return result, nil
}

// InferPromoteSource returns the source repository for a promote --to target.
// It walks the stages list and returns the stage immediately before target.
func (m *Manifest) InferPromoteSource(target string) (string, error) {
	if m.Promote == nil || len(m.Promote.Stages) == 0 {
		return "", fmt.Errorf("manifest has no promote.stages defined")
	}

	for i, stage := range m.Promote.Stages {
		if stage == target {
			if i == 0 {
				return "", fmt.Errorf("cannot promote to %q: it is the first stage", target)
			}
			return m.Promote.Stages[i-1], nil
		}
	}

	return "", fmt.Errorf("stage %q not found in promote.stages %v", target, m.Promote.Stages)
}
