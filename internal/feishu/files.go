package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/os7blue/feishu-codex-bridge/internal/bridge"
)

// The SDK buffers response bodies, so bound reads at the HTTP transport too.
type boundedHTTP struct {
	client interface {
		Do(*http.Request) (*http.Response, error)
	}
}

func (c boundedHTTP) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, errors.New("Feishu transport failed")
	}
	if resp.ContentLength > bridge.MaxFileBytes {
		resp.Body.Close()
		return nil, errors.New("Feishu response exceeds file limit")
	}
	resp.Body = http.MaxBytesReader(nil, resp.Body, bridge.MaxFileBytes)
	return resp, nil
}

func (c *Client) DownloadFile(ctx context.Context, message string, a bridge.Attachment) ([]byte, error) {
	if a.Kind != "file" && a.Kind != "image" {
		return nil, errors.New("unsupported attachment")
	}
	req := larkim.NewGetMessageResourceReqBuilder().MessageId(message).FileKey(a.Key).Type(a.Kind).Build()
	resp, err := c.api.Im.MessageResource.Get(ctx, req)
	if err != nil || resp == nil || !resp.Success() || resp.File == nil {
		code, status := 0, 0
		if resp != nil {
			code = resp.Code
			if resp.ApiResp != nil {
				status = resp.StatusCode
			}
		}
		return nil, fileFailure("download", err, code, status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, bridge.MaxFileBytes+1))
	if err != nil || len(data) == 0 || len(data) > bridge.MaxFileBytes {
		return nil, errors.New("attachment empty or too large")
	}
	return data, nil
}

func (c *Client) SendFile(ctx context.Context, chat, name string, data []byte, key string) ([]string, error) {
	if len(data) == 0 || len(data) > bridge.MaxFileBytes {
		return nil, errors.New("file empty or too large")
	}
	req := larkim.NewCreateFileReqBuilder().Body(larkim.NewCreateFileReqBodyBuilder().FileType("stream").FileName(name).File(bytes.NewReader(data)).Build()).Build()
	resp, err := c.api.Im.File.Create(ctx, req)
	if err != nil || resp == nil || !resp.Success() || resp.Data == nil || value(resp.Data.FileKey) == "" {
		code, status := 0, 0
		if resp != nil {
			code = resp.Code
			if resp.ApiResp != nil {
				status = resp.StatusCode
			}
		}
		return nil, fileFailure("upload", err, code, status)
	}
	body, _ := json.Marshal(map[string]string{"file_key": value(resp.Data.FileKey)})
	id, err := c.sendContent(ctx, chat, "file", string(body), key)
	if err != nil {
		return nil, err
	}
	return []string{id}, nil
}

func fileFailure(operation string, err error, code, status int) error {
	category := "invalid_response"
	if code != 0 {
		category = "api"
	}
	if err != nil {
		category = "transport"
		var apiError *larkcore.CodeError
		if errors.As(err, &apiError) {
			category = "api"
			code = apiError.Code
		}
	}
	return &DeliveryError{Kind: "file_" + operation + "_" + category, Code: code, HTTPStatus: status}
}
