package manifest

import (
	"fmt"
	"strings"

	"github.com/jmurray2011/cob/pkg/cob"
)

// ParseCoordinates parses compact coordinate strings:
//
//	domain/repo                          -> list packages
//	domain/repo/namespace/package        -> list versions
//	domain/repo/namespace/package@ver    -> specific version
//
// Wildcard repo (*) is supported for ls across repos.
func ParseCoordinates(s string) (*cob.PackageCoordinates, error) {
	s = strings.TrimRight(s, "/")

	// Split off @version if present
	var version string
	if idx := strings.LastIndex(s, "@"); idx != -1 {
		version = s[idx+1:]
		s = s[:idx]
		if version == "" {
			return nil, fmt.Errorf("empty version after @")
		}
	}

	parts := strings.Split(s, "/")
	switch len(parts) {
	case 1:
		return &cob.PackageCoordinates{
			Domain:  parts[0],
			Version: version,
		}, nil
	case 2:
		return &cob.PackageCoordinates{
			Domain:     parts[0],
			Repository: parts[1],
			Version:    version,
		}, nil
	case 3:
		return nil, fmt.Errorf("got 3 segments (%s/%s/%s) — expected domain/repo or domain/repo/namespace/package. Missing namespace?", parts[0], parts[1], parts[2])
	case 4:
		return &cob.PackageCoordinates{
			Domain:     parts[0],
			Repository: parts[1],
			Namespace:  parts[2],
			Package:    parts[3],
			Version:    version,
		}, nil
	default:
		return nil, fmt.Errorf("expected domain, domain/repo, domain/repo/package, or domain/repo/namespace/package, got %d segments", len(parts))
	}
}

// FormatCoordinates returns the compact string representation.
func FormatCoordinates(c *cob.PackageCoordinates) string {
	var b strings.Builder
	if c.Namespace != "" && c.Package != "" {
		fmt.Fprintf(&b, "%s/%s/%s/%s", c.Domain, c.Repository, c.Namespace, c.Package)
	} else {
		fmt.Fprintf(&b, "%s/%s", c.Domain, c.Repository)
	}
	if c.Version != "" {
		fmt.Fprintf(&b, "@%s", c.Version)
	}
	return b.String()
}
