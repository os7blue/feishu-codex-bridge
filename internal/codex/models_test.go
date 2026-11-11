package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const catalogModel = `{"id":"alias","model":"model-a","isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}`

func TestModelCatalogPaginationAndInferenceBoundary(t *testing.T) {
	calls := 0
	models, e := ListModels(context.Background(), func(_ context.Context, method string, p any) (json.RawMessage, error) {
		calls++
		if method != "model/list" {
			t.Fatal("inference requested")
		}
		params := p.(map[string]any)
		if calls == 1 {
			return json.RawMessage(`{"data":[` + catalogModel + `],"nextCursor":"page2"}`), nil
		}
		if params["cursor"] != "page2" {
			t.Fatal("cursor lost")
		}
		return json.RawMessage(`{"data":[{"hidden":true}],"nextCursor":null}`), nil
	})
	if e != nil || calls != 2 || len(models) != 1 || models[0].Model != "model-a" {
		t.Fatal(models, e)
	}
}
func TestInvalidCatalogDoesNotGuessOrExposeProviderError(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":[]}`, `{"data":[{"id":"bad"}]}`, `{"data":[` + catalogModel + `,` + catalogModel + `]}`, `{"data":[],"nextCursor":"repeat"}`} {
		if _, e := ListModels(context.Background(), func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(body), nil }); e == nil {
			t.Fatal("invalid catalog accepted", body)
		}
	}
	_, e := ListModels(context.Background(), func(context.Context, string, any) (json.RawMessage, error) {
		return nil, errors.New("synthetic-private-provider-text")
	})
	if e == nil || strings.Contains(e.Error(), "synthetic-private") {
		t.Fatal("unsafe error", e)
	}
}
