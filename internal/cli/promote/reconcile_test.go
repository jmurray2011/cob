package promote

import (
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
)

func TestReconcilePromotedAssets(t *testing.T) {
	recorded := []cob.ProvenanceEntry{
		{Asset: "app.bin", Key: "app", Source: "s3://b/app", SHA256: "old", Origin: &cob.Origin{Type: "s3"}},
		{Asset: "gone.bin", Key: "gone"}, // recorded but no longer present
	}
	results := []*cob.AssetResult{
		{Name: "app.bin", SHA256: "new", Size: 10},
		{Name: "extra.bin", SHA256: "x", Size: 5}, // promoted, never recorded
	}
	got := reconcilePromotedAssets(recorded, []string{"app.bin", "extra.bin"}, results)

	if len(got) != 2 {
		t.Fatalf("got %d entries, want exactly the 2 promoted assets", len(got))
	}
	// app.bin: hash/size from the actual promote, Origin/Source from the record.
	if got[0].Asset != "app.bin" || got[0].SHA256 != "new" || got[0].Size != 10 {
		t.Errorf("app.bin not rebuilt from results: %+v", got[0])
	}
	if got[0].Source != "s3://b/app" || got[0].Origin == nil {
		t.Errorf("app.bin lost recorded Origin/Source: %+v", got[0])
	}
	// extra.bin: added out-of-band, still gets an honest entry.
	if got[1].Asset != "extra.bin" || got[1].SHA256 != "x" {
		t.Errorf("extra.bin not recorded: %+v", got[1])
	}
	// gone.bin: recorded but not promoted -> must not appear.
	for _, e := range got {
		if e.Asset == "gone.bin" {
			t.Error("gone.bin was not promoted; it must not appear in provenance")
		}
	}
}
