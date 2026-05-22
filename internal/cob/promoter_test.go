package cob

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"
)

func TestListAssetsToPromote(t *testing.T) {
	ctx := context.Background()

	t.Run("paginates and collects all names", func(t *testing.T) {
		ca := &fakeCA{listAssetsFn: func(in *codeartifact.ListPackageVersionAssetsInput) (*codeartifact.ListPackageVersionAssetsOutput, error) {
			if in.NextToken == nil {
				return &codeartifact.ListPackageVersionAssetsOutput{
					Assets:    []catypes.AssetSummary{{Name: aws.String("a")}, {Name: aws.String("b")}},
					NextToken: aws.String("p2"),
				}, nil
			}
			return &codeartifact.ListPackageVersionAssetsOutput{
				Assets: []catypes.AssetSummary{{Name: aws.String("c")}},
			}, nil
		}}
		names, err := NewPromoter(newTestClient(ca)).ListAssetsToPromote(ctx, coords(), "dev")
		if err != nil {
			t.Fatalf("ListAssetsToPromote: %v", err)
		}
		if strings.Join(names, ",") != "a,b,c" {
			t.Fatalf("got %v, want [a b c]", names)
		}
	})

	t.Run("empty -> guidance error", func(t *testing.T) {
		_, err := NewPromoter(newTestClient(&fakeCA{})).ListAssetsToPromote(ctx, coords(), "dev")
		if err == nil {
			t.Fatal("expected error when source repo has no assets for the version")
		}
	})
}

func TestPromoteAssetPassesUnfinished(t *testing.T) {
	for _, unfinished := range []bool{true, false} {
		var captured *codeartifact.PublishPackageVersionInput
		ca := &fakeCA{
			getAssetFn: func(*codeartifact.GetPackageVersionAssetInput) (*codeartifact.GetPackageVersionAssetOutput, error) {
				return &codeartifact.GetPackageVersionAssetOutput{
					Asset: io.NopCloser(strings.NewReader("data")),
				}, nil
			},
			publishFn: func(in *codeartifact.PublishPackageVersionInput) (*codeartifact.PublishPackageVersionOutput, error) {
				captured = in
				return &codeartifact.PublishPackageVersionOutput{}, nil
			},
		}
		res, err := NewPromoter(newTestClient(ca)).
			PromoteAsset(context.Background(), coords(), "dev", "staging", "thing.bin", unfinished)
		if err != nil {
			t.Fatalf("PromoteAsset: %v", err)
		}
		if aws.ToBool(captured.Unfinished) != unfinished {
			t.Errorf("Unfinished = %v, want %v", aws.ToBool(captured.Unfinished), unfinished)
		}
		if aws.ToString(captured.AssetName) != "thing.bin" {
			t.Errorf("AssetName = %q, want thing.bin", aws.ToString(captured.AssetName))
		}
		if res.Size != int64(len("data")) {
			t.Errorf("Size = %d, want %d", res.Size, len("data"))
		}
	}
}
