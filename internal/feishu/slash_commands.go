package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/os7blue/feishu-codex-bridge/internal/commandset"
)

const slashCommandPath = "/open-apis/application/v7/app_slash_commands"

// SlashError contains only allowlisted diagnostics. Never wrap SDK errors or
// retain API messages, headers, response bodies or command IDs in reports.
type SlashError struct {
	Kind       string `json:"kind"`
	Code       int    `json:"code,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *SlashError) Error() string {
	return fmt.Sprintf("native commands: %s (code=%d, http_status=%d)", e.Kind, e.Code, e.HTTPStatus)
}

type SlashCommandStatus struct {
	Name   string `json:"command"`
	Status string `json:"status"`
}

type SlashReport struct {
	Commands []SlashCommandStatus `json:"commands"`
	Verified bool                 `json:"verified"`
}

func (c *Client) slashRequest(ctx context.Context, method string, body any) (json.RawMessage, error) {
	response, err := c.api.Do(ctx, &larkcore.ApiReq{
		HttpMethod: method, ApiPath: slashCommandPath, Body: body,
		SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant},
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, &SlashError{Kind: "canceled"}
		}
		// Token acquisition can fail before the raw API response is available.
		var apiError *larkcore.CodeError
		if errors.As(err, &apiError) {
			return nil, &SlashError{Kind: "api", Code: apiError.Code}
		}
		return nil, &SlashError{Kind: "transport"}
	}
	if response == nil {
		return nil, &SlashError{Kind: "invalid_response"}
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if len(response.RawBody) > 1<<20 || json.Unmarshal(response.RawBody, &envelope) != nil || envelope.Code == nil {
		return nil, &SlashError{Kind: "invalid_response", HTTPStatus: response.StatusCode}
	}
	if *envelope.Code != 0 || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &SlashError{Kind: "api", Code: *envelope.Code, HTTPStatus: response.StatusCode}
	}
	return envelope.Data, nil
}

func (c *Client) slashNames(ctx context.Context) (map[string]bool, error) {
	data, err := c.slashRequest(ctx, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []struct {
			Command string `json:"command"`
		} `json:"items"`
		HasMore       bool   `json:"has_more"`
		PageToken     string `json:"page_token"`
		NextPageToken string `json:"next_page_token"`
		Total         *int   `json:"total"`
	}
	// The official endpoint lists all commands without pagination. Reject an
	// incomplete or unfamiliar response rather than assuming commands are absent.
	if json.Unmarshal(data, &result) != nil || result.Items == nil || len(result.Items) > 100 || result.HasMore || result.PageToken != "" || result.NextPageToken != "" {
		return nil, &SlashError{Kind: "incomplete_list"}
	}
	if result.Total != nil && *result.Total != len(result.Items) {
		return nil, &SlashError{Kind: "incomplete_list"}
	}
	names := make(map[string]bool, len(result.Items))
	for _, item := range result.Items {
		if strings.TrimSpace(item.Command) == "" || names[item.Command] {
			return nil, &SlashError{Kind: "invalid_list"}
		}
		names[item.Command] = true
	}
	return names, nil
}

// SlashCommands lists first and only creates absent roots when sync is true.
// Existing definitions and unrelated commands are never updated or deleted.
// The CLI does not open bridge state, connect a websocket or start Codex.
func (c *Client) SlashCommands(ctx context.Context, sync bool) (SlashReport, error) {
	catalog := commandset.All()
	report := SlashReport{Commands: make([]SlashCommandStatus, len(catalog))}
	for i, command := range catalog {
		report.Commands[i] = SlashCommandStatus{Name: command.Name, Status: "unchecked"}
	}
	names, err := c.slashNames(ctx)
	if err != nil {
		return report, err
	}
	missing := 0
	for i, command := range catalog {
		report.Commands[i].Status = "present"
		if !names[command.Name] {
			report.Commands[i].Status = "missing"
			missing++
		}
	}
	if !sync {
		report.Verified = missing == 0
		return report, nil
	}
	if len(names)+missing > 100 {
		return report, &SlashError{Kind: "capacity"}
	}
	for i, command := range catalog {
		if names[command.Name] {
			continue
		}
		report.Commands[i].Status = "unconfirmed"
		_, createErr := c.slashRequest(ctx, http.MethodPost, map[string]any{
			"command":     command.Name,
			"description": map[string]string{"default_value": command.Description},
		})
		// Read after each POST, including ambiguous failures and name collisions.
		// Never blindly retry a write. A confirmed same-name root is preserved.
		current, listErr := c.slashNames(ctx)
		if listErr != nil {
			return report, listErr
		}
		if !current[command.Name] {
			if createErr != nil {
				return report, createErr
			}
			return report, &SlashError{Kind: "write_not_visible"}
		}
		if createErr == nil {
			report.Commands[i].Status = "created"
		} else {
			report.Commands[i].Status = "present"
		}
		// Refresh all names: another registrar may have added later roots too.
		names = current
	}
	// The last complete read must contain every root, even if another actor
	// removed an earlier command while this run was adding missing entries.
	for i, command := range catalog {
		if !names[command.Name] {
			report.Commands[i].Status = "missing"
			return report, &SlashError{Kind: "verification_failed"}
		}
		if report.Commands[i].Status == "missing" {
			report.Commands[i].Status = "present"
		}
	}
	report.Verified = true
	return report, nil
}
