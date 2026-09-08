package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
)

func profileOpenAIJSONMarshal(ctx context.Context, value any) ([]byte, error) {
	defer requestprofile.Start(ctx, "json_serialize")()
	return marshalOpenAIUpstreamJSON(value)
}
func profileOpenAIPatches(ctx context.Context, view openAIRequestView) ([]byte, error) {
	defer requestprofile.Start(ctx, "json_patch")()
	return view.ApplyPatches()
}
