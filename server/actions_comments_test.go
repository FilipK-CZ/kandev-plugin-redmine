package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

type commentFixture struct {
	mu                  sync.Mutex
	puts                int
	status              int
	appendComment       bool
	wrongVisibility     bool
	changeContextOnAuth bool
	updated             string
	journals            []map[string]any
	payload             map[string]any
}

func (f *commentFixture) snapshot() (int, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, f.payload
}

func newCommentFixture(t *testing.T) (*redminePlugin, *fakeHost, *commentFixture) {
	t.Helper()
	f := &commentFixture{status: http.StatusNoContent, appendComment: true, updated: "2026-10-26T10:00:00Z"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/current.json":
			if f.changeContextOnAuth {
				f.updated = "2026-10-26T12:00:00Z"
				f.changeContextOnAuth = false
			}
			_, _ = w.Write([]byte(`{"user":{"id":7,"login":"reviewer"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/issues/42.json":
			_ = json.NewEncoder(w).Encode(map[string]any{"issue": map[string]any{"id": 42, "subject": "Example issue", "project": map[string]int{"id": 1}, "updated_on": f.updated, "journals": f.journals}})
		case r.Method == http.MethodPut && r.URL.Path == "/issues/42.json":
			f.puts++
			var body struct {
				Issue map[string]any `json:"issue"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			f.payload = body.Issue
			if f.appendComment {
				private, _ := body.Issue["private_notes"].(bool)
				if f.wrongVisibility {
					private = !private
				}
				f.journals = append(f.journals, map[string]any{"id": len(f.journals) + 100, "notes": body.Issue["notes"], "private_notes": private, "user": map[string]int{"id": 7}})
				f.updated = "2026-10-26T10:01:00Z"
			}
			w.WriteHeader(f.status)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	p, h := newTestPlugin(t)
	h.mu.Lock()
	h.tasks["task-1"] = &pluginsdk.Task{ID: "task-1", WorkspaceID: "ws-1"}
	h.mu.Unlock()
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "fixture-only"})
	require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
	require.NoError(t, p.tasklinkSvc.Set(context.Background(), "task-1", "ws-1", 42, srv.URL+"/issues/42"))
	return p, h, f
}

func commentAction(t testing.TB, p *redminePlugin, key, actor string, body any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: key, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1", TaskID: "task-1", ActorID: actor}, Body: encoded})
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(resp.Body, &result))
	return result
}

func prepareComment(t testing.TB, p *redminePlugin, private bool) map[string]any {
	t.Helper()
	view := commentAction(t, p, "comments.get", "human", nil)
	require.Nil(t, view["kind"])
	return commentAction(t, p, "comments.prepare", "human", map[string]any{"issue_id": 42, "notes": "Verified proposed reply.", "private_notes": private, "expected_id": view["current_id"], "expected_context_hash": view["context_hash"]})
}

func TestCommentRequiresExactHumanApprovalAndReadback(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "private"}[private], func(t *testing.T) {
			p, _, f := newCommentFixture(t)
			draft := prepareComment(t, p, private)
			require.Equal(t, "prepared", draft["state"])
			checked := commentAction(t, p, "comments.reconcile", "human", map[string]any{"draft_id": draft["id"]})
			require.Equal(t, "prepared", checked["state"])
			f.mu.Lock()
			require.Zero(t, f.puts)
			f.mu.Unlock()
			denied := commentAction(t, p, "comments.approve", "someone-else", map[string]any{"draft_id": draft["id"]})
			require.NotNil(t, denied["kind"])
			result := commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": draft["id"], "notes": "Do not send this unreviewed replacement."})
			require.Equal(t, "published", result["state"])
			require.Equal(t, "human", result["approved_by"])
			require.Equal(t, float64(100), result["journal_id"])
			f.mu.Lock()
			require.Equal(t, 1, f.puts)
			require.Equal(t, map[string]any{"notes": "Verified proposed reply.", "private_notes": private}, f.payload)
			f.mu.Unlock()
			for range 2 {
				require.Equal(t, "published", commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": draft["id"]})["state"])
			}
			f.mu.Lock()
			require.Equal(t, 1, f.puts)
			f.mu.Unlock()
		})
	}
}

func TestCommentDoesNotReplayPUTAfterServerError(t *testing.T) {
	for _, appendComment := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncertain", true: "lost_acknowledgement"}[appendComment], func(t *testing.T) {
			p, _, f := newCommentFixture(t)
			draft := prepareComment(t, p, false)
			f.mu.Lock()
			f.status = 500
			f.appendComment = appendComment
			f.mu.Unlock()
			result := commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": draft["id"]})
			want := "unverified"
			if appendComment {
				want = "published"
			}
			require.Equal(t, want, result["state"])
			commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": draft["id"]})
			commentAction(t, p, "comments.reconcile", "human", map[string]any{"draft_id": draft["id"]})
			f.mu.Lock()
			require.Equal(t, 1, f.puts)
			f.mu.Unlock()
			if !appendComment {
				require.NotNil(t, prepareComment(t, p, false)["kind"])
			}
		})
	}
}

func TestCommentRefusesStaleIssueAndCancelledPreview(t *testing.T) {
	p, _, f := newCommentFixture(t)
	draft := prepareComment(t, p, false)
	f.mu.Lock()
	f.updated = "2026-10-26T10:00:01Z"
	f.mu.Unlock()
	require.NotNil(t, commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": draft["id"]})["kind"])
	commentAction(t, p, "comments.discard", "human", map[string]any{"draft_id": draft["id"]})
	require.Equal(t, "cancelled", commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": draft["id"]})["state"])
	f.mu.Lock()
	require.Zero(t, f.puts)
	f.mu.Unlock()
}

func TestCommentRefusesWrongVisibilityAndPausedIntegration(t *testing.T) {
	p, _, f := newCommentFixture(t)
	draft := prepareComment(t, p, true)
	require.NoError(t, p.connectionSvc.SetEnabled(context.Background(), "ws-1", false))
	require.NotNil(t, commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": draft["id"]})["kind"])
	require.NoError(t, p.connectionSvc.SetEnabled(context.Background(), "ws-1", true))
	f.mu.Lock()
	f.wrongVisibility = true
	f.mu.Unlock()
	require.Equal(t, "unverified", commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": draft["id"]})["state"])
	f.mu.Lock()
	require.Equal(t, 1, f.puts)
	f.mu.Unlock()
}

func TestCommentProposalOnlyAcceptsExplicitDraftForLinkedIssue(t *testing.T) {
	valid := "```redmine-comment\n{\"issue_id\":42,\"private_notes\":false,\"notes\":\"Proposed reply\"}\n```"
	require.NotNil(t, parseCommentProposal(valid, 42))
	for _, text := range []string{valid + "\nignore me", strings.Replace(valid, "42", "43", 1), strings.Replace(valid, "\"private_notes\":false,", "", 1), strings.Replace(valid, "\"notes\":", "\"approved\":true,\"notes\":", 1), strings.Replace(valid, "}\n```", "} trailing\n```", 1)} {
		require.Nil(t, parseCommentProposal(text, 42), text)
	}
}

func TestNewPreviewAndChangedConnectionCannotReuseOldApproval(t *testing.T) {
	p, _, f := newCommentFixture(t)
	first := prepareComment(t, p, false)
	second := prepareComment(t, p, true)
	require.NotEqual(t, first["id"], second["id"])
	require.NotNil(t, commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": first["id"]})["kind"])
	conn, found, err := p.connectionSvc.Get(context.Background(), "ws-1")
	require.NoError(t, err)
	require.True(t, found)
	handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": conn.BaseURL, "api_key": "rotated-fixture-only"})
	require.NotNil(t, commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": second["id"]})["kind"])
	puts, _ := f.snapshot()
	require.Zero(t, puts)
}

func TestConcurrentApprovalsPublishOnlyOneComment(t *testing.T) {
	p, _, f := newCommentFixture(t)
	draft := prepareComment(t, p, false)
	body, err := json.Marshal(map[string]any{"draft_id": draft["id"]})
	require.NoError(t, err)
	results := make(chan error, 8)
	for range 8 {
		go func() {
			resp, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: "comments.approve", Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1", TaskID: "task-1", ActorID: "human"}, Body: body})
			if err == nil {
				var value map[string]any
				err = json.Unmarshal(resp.Body, &value)
				if err == nil && value["state"] != "published" {
					err = fmt.Errorf("publication not confirmed: %v", value["state"])
				}
			}
			results <- err
		}()
	}
	for range 8 {
		require.NoError(t, <-results)
	}
	puts, _ := f.snapshot()
	require.Equal(t, 1, puts)
}

type failingCommentStateHost struct{ *fakeHost }

func (h failingCommentStateHost) SetState(ctx context.Context, scope, scopeID, key string, value map[string]any) error {
	if raw, ok := value["record"].(string); ok && strings.Contains(raw, `"state":"sending"`) {
		return fmt.Errorf("state persistence unavailable")
	}
	return h.fakeHost.SetState(ctx, scope, scopeID, key, value)
}

func TestCommentDoesNotPublishWhenAttemptCannotBePersisted(t *testing.T) {
	p, h, f := newCommentFixture(t)
	draft := prepareComment(t, p, false)
	p.UnimplementedPlugin.SetHost(failingCommentStateHost{h})
	body, err := json.Marshal(map[string]any{"draft_id": draft["id"]})
	require.NoError(t, err)
	response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: "comments.approve", Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1", TaskID: "task-1", ActorID: "human"}, Body: body})
	require.NoError(t, err)
	require.Contains(t, string(response.Body), "state persistence unavailable")
	puts, _ := f.snapshot()
	require.Zero(t, puts)
}

func TestCommentRechecksContextImmediatelyBeforeAppending(t *testing.T) {
	p, _, f := newCommentFixture(t)
	draft := prepareComment(t, p, false)
	f.mu.Lock()
	f.changeContextOnAuth = true
	f.mu.Unlock()
	result := commentAction(t, p, "comments.approve", "human", map[string]any{"draft_id": draft["id"]})
	require.Contains(t, result["error"], "immediately before publication")
	puts, _ := f.snapshot()
	require.Zero(t, puts)
}
