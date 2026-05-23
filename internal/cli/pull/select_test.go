package pull

import (
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
)

func TestSelectAssets(t *testing.T) {
	all := []cob.AssetInfo{{Name: "a.bin"}, {Name: "b.bin"}, {Name: "c.bin"}}

	t.Run("no filter returns everything", func(t *testing.T) {
		got, un, err := selectAssets(all, "", "")
		if err != nil || len(got) != 3 || len(un) != 0 {
			t.Fatalf("got %d assets, unmatched %v, err %v", len(got), un, err)
		}
	})
	t.Run("positional asset", func(t *testing.T) {
		got, _, err := selectAssets(all, "b.bin", "")
		if err != nil || len(got) != 1 || got[0].Name != "b.bin" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	})
	t.Run("positional miss is an error", func(t *testing.T) {
		if _, _, err := selectAssets(all, "nope.bin", ""); err == nil {
			t.Fatal("expected a not-found error")
		}
	})
	t.Run("comma filter, trims spaces, reports a miss", func(t *testing.T) {
		got, un, err := selectAssets(all, "", "a.bin, nope.bin ,c.bin")
		if err != nil {
			t.Fatalf("err %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d assets, want 2 (a.bin, c.bin)", len(got))
		}
		if len(un) != 1 || un[0] != "nope.bin" {
			t.Errorf("unmatched = %v, want [nope.bin]", un)
		}
	})
	t.Run("comma filter matching nothing is an error", func(t *testing.T) {
		if _, _, err := selectAssets(all, "", "x,y"); err == nil {
			t.Fatal("expected an error when nothing matched")
		}
	})
}
