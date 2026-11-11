package codex

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/modelsettings"
)

// ListModels adapts App Server metadata to the application catalog port. It
// neither launches a client nor calls turn/start. The owner supplies its RPC.
func ListModels(ctx context.Context, call func(context.Context, string, any) (json.RawMessage, error)) ([]modelsettings.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var models []modelsettings.Model
	cursor := ""
	seen, names := map[string]bool{}, map[string]bool{}
	for page := 0; page < 20; page++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := call(ctx, "model/list", params)
		var result struct {
			Data []struct {
				ID            string `json:"id"`
				Model         string `json:"model"`
				DefaultEffort string `json:"defaultReasoningEffort"`
				Efforts       []struct {
					Effort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
				Default bool `json:"isDefault"`
				Hidden  bool `json:"hidden"`
			} `json:"data"`
			Next *string `json:"nextCursor"`
		}
		if err != nil || json.Unmarshal(raw, &result) != nil || result.Data == nil {
			return nil, errors.New("无法获取可用模型和思考强度。")
		}
		for _, wire := range result.Data {
			if wire.Hidden {
				continue
			}
			m := modelsettings.Model{ID: wire.ID, Model: wire.Model, DefaultEffort: wire.DefaultEffort, Default: wire.Default}
			for _, e := range wire.Efforts {
				if !modelsettings.ValidToken(e.Effort) {
					return nil, errors.New("模型目录的思考强度无效。")
				}
				m.Efforts = append(m.Efforts, e.Effort)
			}
			if !modelsettings.ValidToken(m.Model) || !modelsettings.ValidToken(m.ID) || !m.Supports(m.DefaultEffort) || names[m.Model] || names[m.ID] {
				return nil, errors.New("模型目录不完整或重复，未猜测默认设置。")
			}
			names[m.Model] = true
			names[m.ID] = true
			models = append(models, m)
		}
		if result.Next == nil || *result.Next == "" {
			if len(models) == 0 {
				return nil, errors.New("当前账号没有可用模型。")
			}
			return models, nil
		}
		cursor = *result.Next
		if seen[cursor] {
			return nil, errors.New("模型目录分页重复。")
		}
		seen[cursor] = true
	}
	return nil, errors.New("模型目录分页超过限制。")
}
