package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SourceEntry is an asset name + source URI pair, preserving manifest order.
type SourceEntry struct {
	Name string
	URI  string
}

// Manifest represents a parsed cob manifest file.
type Manifest struct {
	Domain     string         `yaml:"domain"`
	Repository string         `yaml:"repository"`
	Namespace  string         `yaml:"namespace"`
	Package    string         `yaml:"package"`
	Promote    *PromoteConfig `yaml:"promote,omitempty"`

	// Sources preserves the order from the YAML file.
	Sources []SourceEntry `yaml:"-"`

	// Dir is the directory containing the manifest file.
	// Used for resolving relative paths in sources.
	Dir string `yaml:"-"`

	// Overrides records which manifest fields were overridden by env vars.
	Overrides []EnvOverride `yaml:"-"`
}

// PromoteConfig holds the promotion stage list.
type PromoteConfig struct {
	Stages []string `yaml:"stages"`
}

// Load reads and parses a manifest file from disk.
// Source key order from the YAML is preserved in Sources.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	// First pass: decode the scalar fields normally.
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w", path, err)
	}

	// Second pass: parse sources as a yaml.Node to preserve key order.
	sources, err := parseOrderedSources(data)
	if err != nil {
		return nil, fmt.Errorf("parsing sources in %s: %w", path, err)
	}
	m.Sources = sources

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	m.Dir = filepath.Dir(abs)

	m.Overrides = m.applyEnvOverrides()

	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest %s: %w", path, err)
	}

	return &m, nil
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

// parseOrderedSources extracts the "sources" mapping from raw YAML,
// preserving the key order from the file.
func parseOrderedSources(data []byte) ([]SourceEntry, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("unexpected YAML structure")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping at top level")
	}

	// Find the "sources" key.
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == "sources" {
			srcNode := root.Content[i+1]
			if srcNode.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("sources must be a mapping")
			}
			var entries []SourceEntry
			for j := 0; j < len(srcNode.Content)-1; j += 2 {
				entries = append(entries, SourceEntry{
					Name: srcNode.Content[j].Value,
					URI:  srcNode.Content[j+1].Value,
				})
			}
			return entries, nil
		}
	}
	return nil, nil
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
			envKey := varName[4:]
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
