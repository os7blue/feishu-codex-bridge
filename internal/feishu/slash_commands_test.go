package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/os7blue/feishu-codex-bridge/internal/commandset"
)

func slashFixture(t *testing.T, logs io.Writer, handle func(*http.Request) (*http.Response, error)) *Client {
	t.Helper()
	log := slog.New(slog.NewTextHandler(logs, nil))
	client := New("fixture-slash-"+t.Name(), "fixture-secret", log)
	client.api = lark.NewClient(client.appID, client.secret, lark.WithLogger(sdkLog{log}),
		lark.WithHttpClient(fixtureHTTP{handle: func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
				return slashResponse(r, 200, `{"code":0,"tenant_access_token":"fixture-token","expire":7200}`), nil
			}
			if r.URL.Path != slashCommandPath || r.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Fatal("unexpected endpoint or SDK tenant authentication")
			}
			return handle(r)
		}}))
	return client
}

func slashResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func slashList(r *http.Request, names map[string]bool) *http.Response {
	items := []map[string]any{}
	keys := []string{}
	for name := range names {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		items = append(items, map[string]any{"command": name, "command_id": "sensitive-id", "description": map[string]string{"default_value": "sensitive-description"}})
	}
	body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"items": items}})
	return slashResponse(r, 200, string(body))
}

func allSlashNamesExcept(except ...string) map[string]bool {
	names := map[string]bool{}
	for _, command := range commandset.All() {
		names[command.Name] = true
	}
	for _, name := range except {
		delete(names, name)
	}
	return names
}

func TestSlashCheckIsReadOnlyAndReportsOnlyCatalogNames(t *testing.T) {
	client := slashFixture(t, io.Discard, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			t.Fatal("check attempted a write or unsupported pagination")
		}
		return slashList(r, map[string]bool{"status": true, "sensitive-other-command": true}), nil
	})
	report, err := client.SlashCommands(context.Background(), false)
	if err != nil || report.Verified || report.Commands[2].Status != "present" || report.Commands[0].Status != "missing" {
		t.Fatalf("unexpected read-only report: %v", err)
	}
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), "sensitive") || strings.Contains(string(data), "fixture-token") {
		t.Fatal("report leaked non-catalog fields")
	}
}

func TestSlashSyncCreatesRootsAndRepeatsWithoutWrites(t *testing.T) {
	names := map[string]bool{"status": true, "external": true}
	writes := 0
	client := slashFixture(t, io.Discard, func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case http.MethodGet:
			return slashList(r, names), nil
		case http.MethodPost:
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 {
				t.Fatal("invalid registration body")
			}
			var name string
			var description struct {
				DefaultValue string `json:"default_value"`
			}
			if json.Unmarshal(body["command"], &name) != nil || json.Unmarshal(body["description"], &description) != nil || names[name] || strings.ContainsAny(name, "/ \t") || description.DefaultValue == "" {
				t.Fatal("registration overwrote a name or used an invalid root/description")
			}
			names[name] = true
			writes++
			return slashResponse(r, 200, `{"code":0,"data":{}}`), nil
		default:
			t.Fatal("registration attempted update or delete")
			return nil, nil
		}
	})
	for i := 0; i < 2; i++ {
		report, err := client.SlashCommands(context.Background(), true)
		if err != nil || !report.Verified || writes != len(commandset.All())-1 || !names["external"] {
			t.Fatalf("sync failed or repeat wrote again: %v", err)
		}
	}
}

func TestSlashSyncRefusesIncompleteListsBeforeWriting(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"code":0}`, `{"code":0,"data":{}}`, `{"code":0,"data":{"items":null}}`,
		`{"code":0,"data":{"items":[],"has_more":true}}`,
		`{"code":0,"data":{"items":[],"next_page_token":"sensitive"}}`,
		`{"code":0,"data":{"items":[],"page_token":"sensitive"}}`,
		`{"code":0,"data":{"items":[],"total":1}}`,
		`{"code":0,"data":{"items":[{}]}}`,
		`{"code":0,"data":{"items":[{"command":"new"},{"command":"new"}]}}`,
		`{"code":0,"data":{"items":"sensitive"}}`,
	} {
		t.Run(fmt.Sprint(len(body))+body[:2], func(t *testing.T) {
			client := slashFixture(t, io.Discard, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatal("wrote after incomplete list")
				}
				return slashResponse(r, 200, body), nil
			})
			if report, err := client.SlashCommands(context.Background(), true); err == nil || report.Verified {
				t.Fatal("incomplete list accepted")
			}
		})
	}
}

func TestSlashErrorsAndSDKLogsAreSanitized(t *testing.T) {
	for _, status := range []int{200, 403} {
		var logs bytes.Buffer
		client := slashFixture(t, &logs, func(r *http.Request) (*http.Response, error) {
			return slashResponse(r, status, `{"code":99991672,"msg":"sensitive-secret diagnostic","error":{"token":"sensitive-token"}}`), nil
		})
		report, err := client.SlashCommands(context.Background(), true)
		var safe *SlashError
		if !errors.As(err, &safe) || safe.Code != 99991672 || safe.HTTPStatus != status || report.Verified {
			t.Fatal("numeric API diagnostic lost")
		}
		data, _ := json.Marshal(report)
		if strings.Contains(err.Error()+logs.String()+string(data), "sensitive") {
			t.Fatal("raw API data escaped")
		}
	}
	var logs bytes.Buffer
	client := slashFixture(t, &logs, func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("sensitive URL and token")
	})
	_, err := client.SlashCommands(context.Background(), false)
	if err == nil || strings.Contains(err.Error()+logs.String(), "sensitive") {
		t.Fatal("transport error escaped")
	}
}

func TestSlashSyncReconcilesCollisionAndAmbiguousWrites(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			names := allSlashNamesExcept("new")
			writes := 0
			client := slashFixture(t, io.Discard, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					return slashList(r, names), nil
				}
				writes++
				names["new"] = true
				if ambiguous {
					return nil, errors.New("sensitive connection interrupted after commit")
				}
				return slashResponse(r, 400, `{"code":40000000,"msg":"command already exists"}`), nil
			})
			report, err := client.SlashCommands(context.Background(), true)
			if err != nil || !report.Verified || writes != 1 || report.Commands[1].Status != "present" {
				t.Fatalf("did not reconcile safely: %v", err)
			}
		})
	}
}

func TestSlashSyncStopsAndPreservesPartialResults(t *testing.T) {
	names := allSlashNamesExcept("sessions", "new", "status")
	writes := 0
	client := slashFixture(t, io.Discard, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return slashList(r, names), nil
		}
		writes++
		if writes == 1 {
			names["sessions"] = true
			return slashResponse(r, 200, `{"code":0}`), nil
		}
		return slashResponse(r, 403, `{"code":99991672,"msg":"sensitive"}`), nil
	})
	report, err := client.SlashCommands(context.Background(), true)
	if err == nil || report.Verified || writes != 2 || report.Commands[0].Status != "created" || report.Commands[1].Status != "unconfirmed" || report.Commands[2].Status != "missing" {
		t.Fatal("failed write continued or lost partial results")
	}
}

func TestSlashSyncStopsWhenReadAfterWriteFails(t *testing.T) {
	reads, writes := 0, 0
	client := slashFixture(t, io.Discard, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			reads++
			if reads == 1 {
				return slashList(r, map[string]bool{}), nil
			}
			return slashResponse(r, 200, `{"code":0,"data":{}}`), nil
		}
		writes++
		return slashResponse(r, 200, `{"code":0}`), nil
	})
	report, err := client.SlashCommands(context.Background(), true)
	if err == nil || report.Verified || writes != 1 || report.Commands[0].Status != "unconfirmed" {
		t.Fatal("write reported as confirmed without readback")
	}
}

func TestSlashSyncChecksCapacityBeforeWriting(t *testing.T) {
	names := map[string]bool{}
	for i := 0; i < 100; i++ {
		names[fmt.Sprintf("external%d", i)] = true
	}
	client := slashFixture(t, io.Discard, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("wrote beyond command capacity")
		}
		return slashList(r, names), nil
	})
	_, err := client.SlashCommands(context.Background(), true)
	var safe *SlashError
	if !errors.As(err, &safe) || safe.Kind != "capacity" {
		t.Fatal("capacity not checked")
	}
}

func TestSlashSyncDetectsConcurrentRemoval(t *testing.T) {
	names := allSlashNamesExcept("new")
	client := slashFixture(t, io.Discard, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return slashList(r, names), nil
		}
		names["new"] = true
		delete(names, "sessions")
		return slashResponse(r, 200, `{"code":0}`), nil
	})
	report, err := client.SlashCommands(context.Background(), true)
	if err == nil || report.Verified || report.Commands[0].Status != "missing" {
		t.Fatal("stale list reported verified")
	}
}
