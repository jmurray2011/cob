package cob

import "testing"

// FuzzNewS3Source checks the s3:// URI parser never panics and never returns
// a nil source alongside a nil error.
func FuzzNewS3Source(f *testing.F) {
	for _, s := range []string{
		"", "s3://", "s3://b", "s3://b/k", "s3://b/k/with/slashes",
		"s3:///k", "s3://b/", "notascheme", "s3://b/k?x=1",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, uri string) {
		src, err := NewS3Source(nil, uri)
		if err == nil && src == nil {
			t.Errorf("NewS3Source(%q): nil source with a nil error", uri)
		}
	})
}

// FuzzNewCASource checks the ca:// URI parser never panics and never returns
// a nil source alongside a nil error.
func FuzzNewCASource(f *testing.F) {
	for _, s := range []string{
		"", "ca://", "ca://d/r/n/p@1.0.0/a.jar", "ca://d/r/n/p@1.0.0/path/to/a",
		"ca://d/r/n/p@/a", "ca://d/r/n/p/a", "ca://@/", "ca://d/r/n/p@1.0.0/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, uri string) {
		src, err := NewCASource(nil, uri)
		if err == nil && src == nil {
			t.Errorf("NewCASource(%q): nil source with a nil error", uri)
		}
	})
}
