package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"kandev-plugin-redmine/internal/fieldmapping"
	redminesync "kandev-plugin-redmine/internal/sync"
	"kandev-plugin-redmine/internal/watch"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// redmineFixtureServer answers just enough of the Redmine JSON API for the
// integration tests below: auth validation, one issue by id (for
// link.set's resolve step), and a PUT handler that records the status_id it
// was sent (for write-back).
func redmineFixtureServer(t *testing.T, onPut func(statusID any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/current.json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user":{"id":1,"login":"alice"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects.json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"projects":[{"id":1,"name":"One"},{"id":2,"name":"Two"}],"total_count":2}`))
		case r.Method == http.MethodGet && r.URL.Path == "/issues/42.json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"issue":{"id":42,"subject":"Fixture issue","project":{"id":1},"status":{"id":2}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/issue_statuses.json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"issue_statuses":[{"id":2,"name":"Open"},{"id":5,"name":"Done"}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/issues/42.json":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if onPut != nil {
				issue, _ := body["issue"].(map[string]any)
				onPut(issue["status_id"])
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestPlugin(t testing.TB) (*redminePlugin, *fakeHost) {
	t.Helper()
	p := &redminePlugin{}
	host := newFakeHost()
	p.SetHost(host)
	t.Cleanup(p.stop)
	return p, host
}

func handle(t *testing.T, p *redminePlugin, key, workspaceID, taskID string, body any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: key,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: workspaceID, TaskID: taskID},
		Body:      encoded,
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(resp.Body, &out))
	return out
}

func TestHandleAction_ConnectionSaveThenGet_RoundTrips(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := redmineFixtureServer(t, nil)

	saved := handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	require.Equal(t, "connected", saved["state"])

	got := handle(t, p, "connection.get", "ws-1", "", nil)
	require.Equal(t, "connected", got["state"])
}

func TestHandleAction_IntegrationEnabledIsWorkspaceScopedAndPreservesConnection(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := redmineFixtureServer(t, nil)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})

	require.Equal(t, true, handle(t, p, "integration.enabled.get", "ws-1", "", nil)["enabled"])
	require.Equal(t, true, handle(t, p, "integration.enabled.get", "ws-2", "", nil)["enabled"])
	require.Equal(t, false, handle(t, p, "integration.enabled.save", "ws-1", "", map[string]any{"enabled": false})["enabled"])
	require.Equal(t, false, handle(t, p, "integration.enabled.get", "ws-1", "", nil)["enabled"])
	require.Equal(t, true, handle(t, p, "integration.enabled.get", "ws-2", "", nil)["enabled"])

	connection := handle(t, p, "connection.get", "ws-1", "", nil)
	require.Equal(t, "connected", connection["state"])
}

func TestHandleAction_ConnectionResponsesNeverSerializeAPIKey(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := redmineFixtureServer(t, nil)
	for _, actionKey := range []string{"connection.save", "connection.get"} {
		var body []byte
		if actionKey == "connection.save" {
			body = []byte(`{"base_url":"` + srv.URL + `","api_key":"test-api-key"}`)
		}
		resp, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
			ActionKey: actionKey,
			Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1"},
			Body:      body,
		})
		require.NoError(t, err)
		require.NotContains(t, string(resp.Body), "api_key")
		require.NotContains(t, string(resp.Body), "test-api-key")
	}
}

func TestHandleAction_ConnectionSave_InvalidKey_ReturnsClassifiedError(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	out := handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "bad-key"})
	require.Equal(t, "invalid_credentials", out["kind"])
}

func TestHandleAction_IssueUploadAndCreateExposeFullWriteContract(t *testing.T) {
	var uploadFilename, uploadContentType string
	var uploadBody []byte
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/current.json":
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
		case "/trackers.json":
			_, _ = w.Write([]byte(`{"trackers":[{"id":2,"name":"Bug"}]}`))
		case "/issue_statuses.json":
			_, _ = w.Write([]byte(`{"issue_statuses":[{"id":3,"name":"Open"}]}`))
		case "/enumerations/issue_priorities.json":
			_, _ = w.Write([]byte(`{"issue_priorities":[{"id":4,"name":"Normal"}]}`))
		case "/uploads.json":
			uploadFilename = r.URL.Query().Get("filename")
			uploadContentType = r.Header.Get("Content-Type")
			uploadBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"upload":{"token":"upload-token"}}`))
		case "/issues.json":
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"issue":{"id":77,"subject":"Created"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := newTestPlugin(t)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
	upload := handle(t, p, "issues.upload", "ws-1", "", map[string]any{"filename": "note.txt", "content_type": "text/plain", "content_base64": base64.StdEncoding.EncodeToString([]byte("hello"))})
	require.Equal(t, "upload-token", upload["token"])
	require.Equal(t, "note.txt", uploadFilename)
	require.Equal(t, "application/octet-stream", uploadContentType)
	require.Equal(t, "hello", string(uploadBody))

	// Feed the upload action response verbatim into create, as a UI caller
	// does. This proves snake_case content_type decodes into issues.Upload.
	created := handle(t, p, "issues.create", "ws-1", "", map[string]any{"project_id": 1, "tracker_id": 2, "status_id": 3, "priority_id": 4, "subject": "Created", "description": "Body", "custom_fields": []any{map[string]any{"id": 9, "name": "Tier", "value": "Gold"}}, "uploads": []any{upload}})
	require.EqualValues(t, 77, created["id"])
	issue, ok := createBody["issue"].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 1, issue["project_id"])
	require.Equal(t, "Created", issue["subject"])
	uploads, ok := issue["uploads"].([]any)
	require.True(t, ok)
	require.Len(t, uploads, 1)
	createdUpload, ok := uploads[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "upload-token", createdUpload["token"])
	require.Equal(t, "note.txt", createdUpload["filename"])
	require.Equal(t, "text/plain", createdUpload["content_type"])
}

func TestHandleAction_IssueUpdateValidatesIDAndSendsFullPayload(t *testing.T) {
	var updateBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/current.json":
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
		case "/issues/23.json":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"issue":{"id":23,"project":{"id":1}}}`))
				return
			}
			require.Equal(t, http.MethodPut, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&updateBody))
			w.WriteHeader(http.StatusNoContent)
		case "/trackers.json":
			_, _ = w.Write([]byte(`{"trackers":[{"id":2,"name":"Bug"}]}`))
		case "/issue_statuses.json":
			_, _ = w.Write([]byte(`{"issue_statuses":[{"id":3,"name":"Open"}]}`))
		case "/enumerations/issue_priorities.json":
			_, _ = w.Write([]byte(`{"issue_priorities":[{"id":4,"name":"Normal"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := newTestPlugin(t)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))

	invalid := handle(t, p, "issues.update", "ws-1", "", map[string]any{"subject": "missing ID"})
	require.Contains(t, invalid["error"], "issue_id")
	updated := handle(t, p, "issues.update", "ws-1", "", map[string]any{"issue_id": 23, "project_id": 1, "tracker_id": 2, "status_id": 3, "priority_id": 4, "subject": "Updated", "description": "Body", "custom_fields": []any{map[string]any{"id": 9, "name": "Tier", "value": "Gold"}}, "uploads": []any{map[string]any{"token": "u", "filename": "note.txt", "content_type": "text/plain"}}})
	require.Equal(t, true, updated["updated"])
	issue, ok := updateBody["issue"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Updated", issue["subject"])
	require.EqualValues(t, 3, issue["status_id"])
	uploads, ok := issue["uploads"].([]any)
	require.True(t, ok)
	require.Equal(t, "text/plain", uploads[0].(map[string]any)["content_type"])
}

func TestHandleAction_IssueUpdatePreservesOmittedAndExplicitEmptyDescription(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/current.json":
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
		case "/issues/23.json":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"issue":{"id":23,"project":{"id":1}}}`))
				return
			}
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			bodies = append(bodies, body)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := newTestPlugin(t)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))

	require.Equal(t, true, handle(t, p, "issues.update", "ws-1", "", map[string]any{"issue_id": 23, "subject": "  Updated  "})["updated"])
	require.Equal(t, true, handle(t, p, "issues.update", "ws-1", "", map[string]any{"issue_id": 23, "description": ""})["updated"])
	first := bodies[0]["issue"].(map[string]any)
	require.Equal(t, "Updated", first["subject"])
	_, hasDescription := first["description"]
	require.False(t, hasDescription)
	second := bodies[1]["issue"].(map[string]any)
	require.Equal(t, "", second["description"])
}

func TestHandleAction_IssueWriteRejectsUntrustedBoundaries(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/current.json":
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
		case "/issues/23.json":
			_, _ = w.Write([]byte(`{"issue":{"id":23,"project":{"id":2}}}`))
		case "/issues/24.json":
			_, _ = w.Write([]byte(`{"issue":{"id":24,"project":{"id":1}}}`))
		case "/trackers.json":
			_, _ = w.Write([]byte(`{"trackers":[{"id":2,"name":"Bug"}]}`))
		case "/issue_statuses.json":
			_, _ = w.Write([]byte(`{"issue_statuses":[{"id":3,"name":"Open"}]}`))
		case "/enumerations/issue_priorities.json":
			_, _ = w.Write([]byte(`{"issue_priorities":[{"id":4,"name":"Normal"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))

	for _, body := range []map[string]any{
		{"project_id": 1, "subject": "   "},
		{"project_id": 0, "subject": "new"},
		{"project_id": 2, "subject": "new"},
		{"project_id": 1, "tracker_id": 9, "subject": "new"},
		{"project_id": 1, "status_id": 9, "subject": "new"},
		{"project_id": 1, "priority_id": 9, "subject": "new"},
		{"project_id": maxSafeActionID + 1, "subject": "new"},
		{"project_id": 1, "tracker_id": maxSafeActionID + 1, "subject": "new"},
		{"project_id": 1, "subject": "new", "custom_fields": []any{map[string]any{"id": -1}}},
		{"project_id": 1, "subject": "new", "custom_fields": []any{map[string]any{"id": maxSafeActionID + 1}}},
		{"project_id": 1, "subject": "new", "custom_fields": []any{map[string]any{"id": 1}, map[string]any{"id": 1}}},
		{"project_id": 1, "subject": "new", "uploads": []any{map[string]any{"token": "", "filename": "x", "content_type": "text/plain"}}},
		{"project_id": 1, "subject": "new", "uploads": []any{map[string]any{"token": " token", "filename": "x", "content_type": "text/plain"}}},
		{"project_id": 1, "subject": "new", "uploads": []any{map[string]any{"token": "t", "filename": "x", "content_type": "not a media type;"}}},
	} {
		require.NotEmpty(t, handle(t, p, "issues.create", "ws-1", "", body)["error"])
	}
	for _, body := range []map[string]any{
		{"issue_id": 0, "subject": "x"},
		{"issue_id": maxSafeActionID + 1, "subject": "x"},
		{"issue_id": 23},
		{"issue_id": 23, "status_id": 3}, // existing issue belongs to project 2.
		{"issue_id": 24, "status_id": 0},
		{"issue_id": 24, "subject": "  "},
		{"issue_id": 24, "priority_id": maxSafeActionID + 1},
		{"issue_id": 24, "priority_id": 9},
		{"issue_id": 24, "custom_fields": []any{map[string]any{"id": 2}, map[string]any{"id": 2}}},
	} {
		require.NotEmpty(t, handle(t, p, "issues.update", "ws-1", "", body)["error"])
	}
}

func TestHandleAction_IssueUploadRejectsInvalidInput(t *testing.T) {
	p, _ := newTestPlugin(t)
	out := handle(t, p, "issues.upload", "ws-1", "", map[string]any{"filename": "", "content_base64": "aGVsbG8="})
	require.Contains(t, out["error"], "filename")
	out = handle(t, p, "issues.upload", "ws-1", "", map[string]any{"filename": "x", "content_base64": "not-base64"})
	require.Contains(t, out["error"], "invalid")
	out = handle(t, p, "issues.upload", "ws-1", "", map[string]any{"filename": "x", "content_base64": ""})
	require.Contains(t, out["error"], "required")
	ok := handle(t, p, "issues.upload", "ws-1", "", map[string]any{"filename": "x", "content_base64": base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentBytes))})
	require.Contains(t, ok["error"], "no connection")
	out = handle(t, p, "issues.upload", "ws-1", "", map[string]any{"filename": "x", "content_base64": base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentBytes+1))})
	require.Contains(t, out["error"], "exceeds")
}

func TestHandleAction_ProjectsSaveThenList_PersistsSelection(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := redmineFixtureServer(t, nil)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})

	handle(t, p, "projects.save", "ws-1", "", map[string]any{"project_ids": []int{1, 2}})
	out := handle(t, p, "projects.list", "ws-1", "", nil)
	selected, ok := out["selected_ids"].([]any)
	require.True(t, ok)
	require.ElementsMatch(t, []any{float64(1), float64(2)}, selected)
}

func TestHandleAction_ProjectsSaveValidatesPaginatedVisibleIDsAndDedupes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/current.json":
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
		case "/projects.json":
			offset := r.URL.Query().Get("offset")
			start := 0
			if offset == "100" {
				start = 100
			}
			if offset == "200" {
				start = 200
			}
			items := make([]map[string]any, 0, 100)
			for id := start + 1; id <= 250 && id <= start+100; id++ {
				items = append(items, map[string]any{"id": id, "name": fmt.Sprintf("P%d", id)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": items, "total_count": 250})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := newTestPlugin(t)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "key"})
	handle(t, p, "projects.save", "ws-1", "", map[string]any{"project_ids": []int{250, 1, 250}})
	got := handle(t, p, "projects.list", "ws-1", "", nil)
	require.Equal(t, []any{float64(1), float64(250)}, got["selected_ids"])
	invalid := handle(t, p, "projects.save", "ws-1", "", map[string]any{"project_ids": []int{251}})
	require.Contains(t, invalid["error"], "not visible")
}

func TestHandleAction_LinkSetThenGet_ResolvesAndPersistsLink(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := redmineFixtureServer(t, nil)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))

	out := handle(t, p, "link.set", "ws-1", "task-1", map[string]any{"reference": "#42"})
	require.Equal(t, true, out["linked"])
	require.EqualValues(t, 42, out["issue_id"])

	got := handle(t, p, "link.get", "ws-1", "task-1", nil)
	require.Equal(t, true, got["linked"])
}

func TestHandleAction_LinkSetRejectsIssueOutsideSelectedProjects(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := redmineFixtureServer(t, nil)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{2}))

	out := handle(t, p, "link.set", "ws-1", "task-1", map[string]any{"reference": "#42"})
	require.Contains(t, out["error"], "not selected")
	got := handle(t, p, "link.get", "ws-1", "task-1", nil)
	require.False(t, got["linked"].(bool))
}

func TestHandleAction_LinkSetRejectsUntrustedIssueReferences(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := redmineFixtureServer(t, nil)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	for _, reference := range []string{"abc42", "#0", "https://other.example/issues/42", srv.URL + "/projects/42", srv.URL + "/issues/999999999999999999999999"} {
		out := handle(t, p, "link.set", "ws-1", "task-1", map[string]any{"reference": reference})
		require.NotEmpty(t, out["error"], reference)
	}
}

func TestParseIssueReferenceAcceptsConfiguredSubpathOnly(t *testing.T) {
	base := "https://redmine.example/redmine"
	id, err := parseIssueReference(base+"/issues/42/", base)
	require.NoError(t, err)
	require.Equal(t, 42, id)
	for _, reference := range []string{"https://redmine.example/issues/42", "https://redmine.example/redmine/projects/42", "https://other.example/redmine/issues/42", "https://user@redmine.example/redmine/issues/42", "https://redmine.example/redmine/issues/42?x=1", "https://redmine.example/redmine/issues/42#notes"} {
		_, err := parseIssueReference(reference, base)
		require.Error(t, err, reference)
	}
}

func TestHandleAction_LinkSetStatusValidatesLiveStatus(t *testing.T) {
	p, _ := newTestPlugin(t)
	var pushed any
	srv := redmineFixtureServer(t, func(statusID any) { pushed = statusID })
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	require.NoError(t, p.tasklinkSvc.Set(context.Background(), "task-1", "ws-1", 42, srv.URL+"/issues/42"))

	for _, statusID := range []int{0, -1, 99} {
		out := handle(t, p, "link.set_status", "ws-1", "task-1", map[string]any{"status_id": statusID})
		require.NotEmpty(t, out["error"])
	}
	out := handle(t, p, "link.set_status", "ws-1", "task-1", map[string]any{"status_id": 5})
	require.Equal(t, true, out["pushed"])
	require.EqualValues(t, 5, pushed)
}

func TestHandleAction_LinkUnset_RemovesLink(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := redmineFixtureServer(t, nil)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
	handle(t, p, "link.set", "ws-1", "task-1", map[string]any{"reference": "#42"})

	handle(t, p, "link.unset", "ws-1", "task-1", nil)
	got := handle(t, p, "link.get", "ws-1", "task-1", nil)
	require.Equal(t, false, got["linked"])
}

func TestOnEvent_TaskMoved_PushesWritebackForLinkedTaskWithMappedStep(t *testing.T) {
	p, host := newTestPlugin(t)

	var pushedStatus any
	srv := redmineFixtureServer(t, func(statusID any) { pushedStatus = statusID })
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
	handle(t, p, "link.set", "ws-1", "task-1", map[string]any{"reference": "#42"})

	require.NoError(t, p.fieldmappingSvc.Save(context.Background(), "ws-1", fieldmapping.Mapping{
		Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 5, WorkflowStepID: "step-done"}},
	}))
	// autoStatusWriteback left at its default (false, per GetOptions' zero value).

	err := p.OnEvent(context.Background(), &pluginsdk.Event{
		EventID: "e1", EventType: "task.moved",
		Payload: map[string]any{"task_id": "task-1", "to_step_id": "step-done"},
	})
	require.NoError(t, err)
	require.Nil(t, pushedStatus, "autoStatusWriteback defaults to false; OnEvent must not push")
	require.Empty(t, host.updateCalls())
}

func TestHandleAction_WorkflowsList_ReturnsWorkflowsWithSteps(t *testing.T) {
	p, _ := newTestPlugin(t)
	out := handle(t, p, "workflows.list", "ws-1", "", nil)
	workflows, ok := out["workflows"].([]any)
	require.True(t, ok)
	require.Len(t, workflows, 1)
	wf, ok := workflows[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "wf-1", wf["id"])
	steps, ok := wf["steps"].([]any)
	require.True(t, ok)
	require.Len(t, steps, 2)
}

type workflowPageResult struct {
	workflows []pluginsdk.Workflow
	pageInfo  *pluginsdk.PageInfo
}

type pagedWorkflowReader struct {
	pages   map[string]workflowPageResult
	cursors []string
}

func (r *pagedWorkflowReader) List(_ context.Context, _ string, page pluginsdk.Page) ([]pluginsdk.Workflow, *pluginsdk.PageInfo, error) {
	r.cursors = append(r.cursors, page.Cursor)
	result, found := r.pages[page.Cursor]
	if !found {
		return nil, nil, fmt.Errorf("unexpected workflow cursor %q", page.Cursor)
	}
	return result.workflows, result.pageInfo, nil
}

func (*pagedWorkflowReader) ListSteps(_ context.Context, workflowID string) ([]pluginsdk.WorkflowStep, error) {
	return []pluginsdk.WorkflowStep{{ID: "step-" + workflowID, Name: "Step " + workflowID}}, nil
}

func workflowPage(prefix string, first, last int) []pluginsdk.Workflow {
	workflows := make([]pluginsdk.Workflow, 0, last-first+1)
	for i := first; i <= last; i++ {
		workflows = append(workflows, pluginsdk.Workflow{ID: fmt.Sprintf("%s-%d", prefix, i), Name: fmt.Sprintf("Workflow %d", i)})
	}
	return workflows
}

func TestHandleAction_WorkflowsListExhaustsCursorPages(t *testing.T) {
	p, host := newTestPlugin(t)
	reader := &pagedWorkflowReader{pages: map[string]workflowPageResult{
		"":       {workflows: workflowPage("wf", 1, 50), pageInfo: &pluginsdk.PageInfo{HasMore: true, NextCursor: "page-2"}},
		"page-2": {workflows: workflowPage("wf", 51, 51), pageInfo: &pluginsdk.PageInfo{}},
	}}
	host.workflowReader = reader

	out := handle(t, p, "workflows.list", "ws-1", "", nil)
	workflows, ok := out["workflows"].([]any)
	require.True(t, ok)
	require.Len(t, workflows, 51)
	require.Equal(t, "wf-51", workflows[50].(map[string]any)["id"])
	require.Equal(t, []string{"", "page-2"}, reader.cursors)
}

func TestValidateMappingWorkflowAcceptsWorkflowBeyondFirstPage(t *testing.T) {
	p, host := newTestPlugin(t)
	reader := &pagedWorkflowReader{pages: map[string]workflowPageResult{
		"":       {workflows: workflowPage("wf", 1, 50), pageInfo: &pluginsdk.PageInfo{HasMore: true, NextCursor: "page-2"}},
		"page-2": {workflows: workflowPage("wf", 51, 51), pageInfo: &pluginsdk.PageInfo{}},
	}}
	host.workflowReader = reader

	err := p.validateMappingWorkflow(context.Background(), "ws-1", fieldmapping.Mapping{WorkflowID: "wf-51"})
	require.NoError(t, err)
	require.Equal(t, []string{"", "page-2"}, reader.cursors)
}

func TestListWorkspaceWorkflowsRejectsRepeatedCursor(t *testing.T) {
	reader := &pagedWorkflowReader{pages: map[string]workflowPageResult{
		"":       {pageInfo: &pluginsdk.PageInfo{HasMore: true, NextCursor: "page-2"}},
		"page-2": {pageInfo: &pluginsdk.PageInfo{HasMore: true, NextCursor: "page-2"}},
	}}

	_, err := listWorkspaceWorkflows(context.Background(), reader, "ws-1")
	require.EqualError(t, err, `workflow pagination repeated cursor "page-2"`)
	require.Equal(t, []string{"", "page-2"}, reader.cursors)
}

func TestHandleAction_WatchesRejectForgedInvalidOrUnselectedInputs(t *testing.T) {
	p, _ := newTestPlugin(t)
	for _, body := range []map[string]any{
		{"project_id": 0, "max_inflight_tasks": 0},
		{"project_id": 1, "max_inflight_tasks": -1},
		{"project_id": 1, "max_inflight_tasks": 0, "tracker_id": -1},
		{"project_id": 1, "max_inflight_tasks": 0, "status_id": -1},
		{"project_id": 99, "max_inflight_tasks": 0},
	} {
		out := handle(t, p, "watches.create", "ws-1", "", body)
		require.NotEmpty(t, out["error"])
	}
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
	out := handle(t, p, "watches.create", "ws-1", "", map[string]any{"project_id": 2, "max_inflight_tasks": 0})
	require.Contains(t, out["error"], "not selected")
}

func TestHandleAction_WatchesValidateLiveFiltersAndSelectedProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/current.json":
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
		case "/projects.json":
			_, _ = w.Write([]byte(`{"projects":[{"id":1,"name":"One"}],"total_count":1}`))
		case "/trackers.json":
			_, _ = w.Write([]byte(`{"trackers":[{"id":3,"name":"Bug"}]}`))
		case "/issue_statuses.json":
			_, _ = w.Write([]byte(`{"issue_statuses":[{"id":4,"name":"Open"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := newTestPlugin(t)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
	require.NoError(t, p.fieldmappingSvc.Save(context.Background(), "ws-1", fieldmapping.Mapping{WorkflowID: "wf-1"}))
	valid := handle(t, p, "watches.create", "ws-1", "", map[string]any{"project_id": 1, "tracker_id": 3, "status_id": 4, "max_inflight_tasks": 0, "enabled": true})
	require.NotEmpty(t, valid["id"])
	for _, body := range []map[string]any{
		{"project_id": 1, "tracker_id": 99, "max_inflight_tasks": 0},
		{"project_id": 1, "status_id": 99, "max_inflight_tasks": 0},
		{"project_id": 1, "max_inflight_tasks": -1},
		{"project_id": 2, "max_inflight_tasks": 0},
	} {
		require.NotEmpty(t, handle(t, p, "watches.create", "ws-1", "", body)["error"])
	}
}

func TestValidateNativeWatchFiltersUsesOnlyAdvertisedFieldsValuesAndOperators(t *testing.T) {
	available := []watchNativeFilterOption{
		{Field: "status_id", Kind: "select", Operators: operatorsForKind("select"), Values: []watchNativeFilterValue{{Value: "2", Name: "New"}}},
		{Field: "subject", Kind: "text", Operators: operatorsForKind("text")},
	}
	require.NoError(t, validateNativeWatchFilters([]watch.Filter{{Field: "status_id", Operator: "=", Value: "2"}, {Field: "subject", Operator: "~", Value: "incident"}}, available))
	for _, filters := range [][]watch.Filter{
		{{Field: "status_id", Operator: "=", Value: "99"}},
		{{Field: "status_id", Operator: "~", Value: "2"}},
		{{Field: "unknown", Operator: "=", Value: "1"}},
		{{Field: "status_id", Operator: "=", Value: "2"}, {Field: "status_id", Operator: "=", Value: "2"}},
	} {
		require.Error(t, validateNativeWatchFilters(filters, available))
	}
}

func TestHandleAction_WatchesPollCreatesTaskImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/current.json":
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
		case "/projects.json":
			_, _ = w.Write([]byte(`{"projects":[{"id":1,"name":"One"}],"total_count":1}`))
		case "/issues.json":
			_, _ = w.Write([]byte(`{"issues":[{"id":42,"subject":"Watched issue"}],"total_count":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, host := newTestPlugin(t)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
	require.NoError(t, p.fieldmappingSvc.Save(context.Background(), "ws-1", fieldmapping.Mapping{WorkflowID: "wf-1"}))
	created := handle(t, p, "watches.create", "ws-1", "", map[string]any{"project_id": 1, "max_inflight_tasks": 1, "enabled": true})
	require.NotEmpty(t, created["id"])

	require.Equal(t, true, handle(t, p, "watches.poll", "ws-1", "", nil)["polled"])
	require.Len(t, host.tasks, 1)
}

func TestHandleAction_FieldMappingSaveValidatesAndNormalizesLiveValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/current.json":
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
		case "/issue_statuses.json":
			_, _ = w.Write([]byte(`{"issue_statuses":[{"id":1,"name":"Server Open","is_closed":false}]}`))
		case "/trackers.json":
			_, _ = w.Write([]byte(`{"trackers":[{"id":2,"name":"Task"}]}`))
		case "/enumerations/issue_priorities.json":
			_, _ = w.Write([]byte(`{"issue_priorities":[{"id":3,"name":"Server High"}]}`))
		case "/custom_fields.json":
			_, _ = w.Write([]byte(`{"custom_fields":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := newTestPlugin(t)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "key"})
	base := map[string]any{"workflow_id": "wf-1", "statuses": []any{map[string]any{"redmine_status_id": 1, "redmine_name": "forged", "is_closed": true, "workflow_step_id": "step-done"}}, "trackers": []any{map[string]any{"redmine_tracker_id": 2, "redmine_name": "legacy", "task_label": "bug"}}, "priorities": []any{map[string]any{"redmine_priority_id": 3, "redmine_name": "forged", "task_priority": "high"}}}
	require.Equal(t, true, handle(t, p, "fieldmapping.save", "ws-1", "", base)["saved"])
	mapping, found, err := p.fieldmappingSvc.Get(context.Background(), "ws-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Server Open", mapping.Statuses[0].RedmineName)
	require.False(t, mapping.Statuses[0].IsClosed)
	encodedMapping, err := json.Marshal(mapping)
	require.NoError(t, err)
	require.NotContains(t, string(encodedMapping), `"trackers"`)
	require.Equal(t, "Server High", mapping.Priorities[0].RedmineName)
	loaded := handle(t, p, "fieldmapping.get", "ws-1", "", nil)
	require.NotContains(t, loaded, "trackers")
	require.Contains(t, loaded, "live_trackers")
	require.Contains(t, loaded, "priorities")
	require.Contains(t, loaded, "live_priorities")
	duplicate := map[string]any{"workflow_id": "wf-1", "statuses": []any{map[string]any{"redmine_status_id": 1}, map[string]any{"redmine_status_id": 1}}}
	require.NotEmpty(t, handle(t, p, "fieldmapping.save", "ws-1", "", duplicate)["error"])
	invalidPriority := map[string]any{"workflow_id": "wf-1", "priorities": []any{map[string]any{"redmine_priority_id": 3, "task_priority": "urgent"}}}
	require.NotEmpty(t, handle(t, p, "fieldmapping.save", "ws-1", "", invalidPriority)["error"])
}

func TestHandleAction_FieldMappingGet_NonAdminDerivesNamedCustomField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/current.json":
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
		case "/issue_statuses.json":
			_, _ = w.Write([]byte(`{"issue_statuses":[]}`))
		case "/trackers.json":
			_, _ = w.Write([]byte(`{"trackers":[]}`))
		case "/enumerations/issue_priorities.json":
			_, _ = w.Write([]byte(`{"issue_priorities":[]}`))
		case "/custom_fields.json":
			w.WriteHeader(http.StatusForbidden)
		case "/issues.json":
			require.Equal(t, "*", r.URL.Query().Get("status_id"))
			_, _ = w.Write([]byte(`{"issues":[
				{"id":1,"custom_fields":[{"id":7,"name":"Customer tier","value":"Gold"}]},
				{"id":2,"custom_fields":[{"id":7,"value":"Silver"}]}
			],"total_count":2}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, _ := newTestPlugin(t)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "key"})
	loaded := handle(t, p, "fieldmapping.get", "ws-1", "", nil)

	require.Equal(t, true, loaded["custom_fields_derived"])
	require.Equal(t, []any{map[string]any{"id": float64(7), "name": "Customer tier"}}, loaded["custom_fields"])
}

func TestHandleAction_SyncOptionsGetRoundTripsAndPreservesOtherToggle(t *testing.T) {
	p, _ := newTestPlugin(t)
	handle(t, p, "syncoptions.save", "ws-1", "", map[string]any{"auto_status_writeback": true, "sync_title_description": false})
	loaded := handle(t, p, "syncoptions.get", "ws-1", "", nil)
	require.Equal(t, true, loaded["auto_status_writeback"])
	require.Equal(t, false, loaded["sync_title_description"])
	// UI saves both loaded values when changing either switch.
	handle(t, p, "syncoptions.save", "ws-1", "", map[string]any{"auto_status_writeback": true, "sync_title_description": true})
	loaded = handle(t, p, "syncoptions.get", "ws-1", "", nil)
	require.Equal(t, true, loaded["auto_status_writeback"])
	require.Equal(t, true, loaded["sync_title_description"])
}

func TestApplyWatchMapping_BackfillsLegacyWatchPlacementAndAttributes(t *testing.T) {
	statusID := 5
	legacy := watch.Watch{ID: "legacy", WorkspaceID: "ws-1", ProjectID: 1, StatusID: &statusID}
	require.True(t, needsWatchBackfill(legacy))
	backfilled := applyWatchMapping(legacy, fieldmapping.Mapping{
		WorkflowID: "wf-secondary",
		Statuses:   []fieldmapping.StatusMapping{{RedmineStatusID: 5, WorkflowStepID: "step-triage"}},
		Priorities: []fieldmapping.PriorityMapping{{RedminePriorityID: 4, TaskPriority: "high"}},
	})
	require.Equal(t, "wf-secondary", backfilled.WorkflowID)
	require.Equal(t, "step-triage", backfilled.WorkflowStepID)
	require.Equal(t, "high", backfilled.PriorityMappings[4])
	require.False(t, needsWatchBackfill(backfilled))
}

func TestApplyWatchPriorityMapping_RefreshesChangedWorkspacePriority(t *testing.T) {
	watchObj := watch.Watch{PriorityMappings: map[int]string{4: "high"}}
	refreshed := applyWatchPriorityMapping(watchObj, fieldmapping.Mapping{
		Priorities: []fieldmapping.PriorityMapping{{RedminePriorityID: 4, TaskPriority: "low"}},
	})

	require.Equal(t, "low", refreshed.PriorityMappings[4])
}

func TestOnEvent_TaskMoved_AutoWritebackEnabled_PushesStatus(t *testing.T) {
	p, _ := newTestPlugin(t)

	var pushedStatus any
	srv := redmineFixtureServer(t, func(statusID any) { pushedStatus = statusID })
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
	handle(t, p, "link.set", "ws-1", "task-1", map[string]any{"reference": "#42"})

	require.NoError(t, p.fieldmappingSvc.Save(context.Background(), "ws-1", fieldmapping.Mapping{
		Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 5, WorkflowStepID: "step-done"}},
	}))
	handle(t, p, "syncoptions.save", "ws-1", "", map[string]any{"auto_status_writeback": true})

	err := p.OnEvent(context.Background(), &pluginsdk.Event{
		EventID: "e1", EventType: "task.moved",
		Payload: map[string]any{"task_id": "task-1", "to_step_id": "step-done"},
	})
	require.NoError(t, err)
	require.EqualValues(t, 5, pushedStatus)
}

func TestOnEvent_TaskMoved_DisabledIntegrationSkipsWriteback(t *testing.T) {
	p, _ := newTestPlugin(t)
	var pushedStatus any
	srv := redmineFixtureServer(t, func(statusID any) { pushedStatus = statusID })
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	handle(t, p, "link.set", "ws-1", "task-1", map[string]any{"reference": "#42"})
	require.NoError(t, p.fieldmappingSvc.Save(context.Background(), "ws-1", fieldmapping.Mapping{
		Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 5, WorkflowStepID: "step-done"}},
	}))
	handle(t, p, "syncoptions.save", "ws-1", "", map[string]any{"auto_status_writeback": true})
	handle(t, p, "integration.enabled.save", "ws-1", "", map[string]any{"enabled": false})

	err := p.OnEvent(context.Background(), &pluginsdk.Event{EventType: "task.moved", Payload: map[string]any{"task_id": "task-1", "to_step_id": "step-done"}})
	require.NoError(t, err)
	require.Nil(t, pushedStatus)
}

func TestOnEvent_WorkspaceDeleted_CleansPluginStateAfterTaskCascade(t *testing.T) {
	p, _ := newTestPlugin(t)
	srv := redmineFixtureServer(t, nil)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "good-key"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
	require.NoError(t, p.fieldmappingSvc.Save(context.Background(), "ws-1", fieldmapping.Mapping{}))
	require.NoError(t, p.syncSvc.SaveOptions(context.Background(), "ws-1", redminesync.Options{AutoStatusWriteback: true}))
	_, err := p.watchSvc.CreateWatch(context.Background(), watch.Watch{WorkspaceID: "ws-1", ProjectID: 1, Enabled: true})
	require.NoError(t, err)
	require.NoError(t, p.tasklinkSvc.Set(context.Background(), "task-already-cascaded", "ws-1", 42, "url"))
	handle(t, p, "integration.enabled.save", "ws-1", "", map[string]any{"enabled": false})

	require.NoError(t, p.OnEvent(context.Background(), &pluginsdk.Event{EventID: "deleted", EventType: "workspace.deleted", WorkspaceID: "ws-1"}))
	_, found, err := p.connectionSvc.Get(context.Background(), "ws-1")
	require.NoError(t, err)
	require.False(t, found)
	watches, err := p.watchSvc.ListWatches(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Empty(t, watches)
	_, found, err = p.tasklinkSvc.Get(context.Background(), "task-already-cascaded")
	require.NoError(t, err)
	require.False(t, found)
	enabled, err := p.connectionSvc.GetEnabled(context.Background(), "ws-1")
	require.NoError(t, err)
	require.True(t, enabled, "workspace deletion removes the preference so a reused id gets the safe default")
}

func TestOnEvent_TaskDeletedIdempotentlyRemovesLink(t *testing.T) {
	p, _ := newTestPlugin(t)
	require.NoError(t, p.tasklinkSvc.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	event := &pluginsdk.Event{EventID: "deleted", EventType: "task.deleted", Payload: map[string]any{"task_id": "task-1"}}
	require.NoError(t, p.OnEvent(context.Background(), event))
	require.NoError(t, p.OnEvent(context.Background(), event))
	_, found, err := p.tasklinkSvc.Get(context.Background(), "task-1")
	require.NoError(t, err)
	require.False(t, found)
}
