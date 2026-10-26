package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

type replyTestWorkflows struct {
	fakeWorkflowReader
	autoStart bool
}

func (r replyTestWorkflows) ListSteps(context.Context, string) ([]pluginsdk.WorkflowStep, error) {
	step := pluginsdk.WorkflowStep{ID: "reply-step", WorkflowID: "wf-1", Name: "Client reply", AgentProfileID: "read-only-profile"}
	if r.autoStart {
		step.OnEnterActionTypes = []string{"auto_start_agent"}
	}
	return []pluginsdk.WorkflowStep{step}, nil
}

type replyMessageHost struct {
	*fakeHost
	mu       sync.Mutex
	sends    []string
	messages []pluginsdk.Message
	sendErr  error
}

func (h *replyMessageHost) Messages() pluginsdk.MessageReader { return replyMessages{host: h} }

type replyMessages struct {
	pluginsdk.MessageReader
	host *replyMessageHost
}

func (r replyMessages) List(_ context.Context, filter pluginsdk.MessageFilter, _ pluginsdk.Page) ([]pluginsdk.Message, *pluginsdk.PageInfo, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	if len(filter.TaskIDs) != 1 || filter.TaskIDs[0] != "task-1" {
		return nil, nil, fmt.Errorf("unexpected message scope")
	}
	return append([]pluginsdk.Message(nil), r.host.messages...), nil, nil
}
func (r replyMessages) Send(_ context.Context, taskID, sessionID, text string) (*pluginsdk.MessageDispatch, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	if taskID != "task-1" || sessionID != "" {
		return nil, fmt.Errorf("unexpected dispatch scope")
	}
	r.host.sends = append(r.host.sends, text)
	if r.host.sendErr != nil {
		return nil, r.host.sendErr
	}
	r.host.messages = append(r.host.messages, pluginsdk.Message{ID: "request", TaskID: taskID, AuthorType: "user", Type: "message", Content: text, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	return &pluginsdk.MessageDispatch{SessionID: "draft-session", Status: "queued"}, nil
}

func newReplyFixture(t *testing.T) (*redminePlugin, *replyMessageHost, *commentFixture) {
	t.Helper()
	p, h, f := newCommentFixture(t)
	h.mu.Lock()
	h.workflowReader = replyTestWorkflows{}
	h.tasks["task-1"].WorkflowID = "wf-1"
	h.tasks["task-1"].WorkflowStepID = "reply-step"
	h.mu.Unlock()
	wrapped := &replyMessageHost{fakeHost: h}
	p.UnimplementedPlugin.SetHost(wrapped)
	settings := handle(t, p, "replies.settings.save", "ws-1", "", replySettings{WorkflowID: "wf-1", StepID: "reply-step", PromptName: "client-reply"})
	require.Nil(t, settings["kind"])
	return p, wrapped, f
}

func movedReplyEvent(id string) *pluginsdk.Event {
	return &pluginsdk.Event{EventID: id, EventType: "task.moved", WorkspaceID: "ws-1", Payload: map[string]any{"task_id": "task-1", "to_step_id": "reply-step"}}
}

func TestReplyEventSendsLivePublicContextOnceWithoutPublishing(t *testing.T) {
	p, h, f := newReplyFixture(t)
	f.mu.Lock()
	f.journals = []map[string]any{{"id": 1, "notes": "Latest public decision"}, {"id": 2, "notes": "Private planning note", "private_notes": true}}
	f.mu.Unlock()
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("event-1")))
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("event-1")))
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("event-2")))
	h.mu.Lock()
	require.Len(t, h.sends, 1)
	prompt := h.sends[0]
	h.mu.Unlock()
	require.True(t, strings.HasPrefix(prompt, "@client-reply\n"))
	require.Contains(t, prompt, "Latest public decision")
	require.NotContains(t, prompt, "Private planning note")
	require.Contains(t, prompt, "```redmine-comment\n{\"issue_id\":42")
	require.Contains(t, prompt, "Publication is controlled by the plugin's workspace setting")
	require.Contains(t, prompt, "do not publish, change issue fields, approve anything")
	f.mu.Lock()
	require.Zero(t, f.puts)
	f.mu.Unlock()
	// An event coalesced during an active request must remain coalesced even
	// if the host redelivers it after the original draft completes.
	h.mu.Lock()
	h.messages = append(h.messages, pluginsdk.Message{TaskID: "task-1", Type: "message", AuthorType: "agent", Content: "```redmine-comment\n{\"issue_id\":42,\"private_notes\":false,\"notes\":\"Proposed response\"}\n```", CreatedAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)})
	h.mu.Unlock()
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("event-2")))
	proposal, _ := p.latestCommentProposal(context.Background(), "task-1", 42)
	require.NotNil(t, proposal)
	h.mu.Lock()
	require.Len(t, h.sends, 1)
	h.mu.Unlock()
}

func TestUncertainReplyDispatchNeverReplays(t *testing.T) {
	p, h, f := newReplyFixture(t)
	h.mu.Lock()
	h.sendErr = fmt.Errorf("lost acknowledgement")
	h.mu.Unlock()
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("event-1")))
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("event-1")))
	require.Error(t, p.OnEvent(context.Background(), movedReplyEvent("event-2")))
	h.mu.Lock()
	require.Len(t, h.sends, 1)
	h.mu.Unlock()
	f.mu.Lock()
	require.Zero(t, f.puts)
	f.mu.Unlock()
}

func TestReplyDraftingRejectsNativeAutoStartAndPausedOrMovedTasks(t *testing.T) {
	p, h, _ := newReplyFixture(t)
	h.fakeHost.mu.Lock()
	h.workflowReader = replyTestWorkflows{autoStart: true}
	h.fakeHost.mu.Unlock()
	result := handle(t, p, "replies.settings.save", "ws-1", "", replySettings{WorkflowID: "wf-1", StepID: "reply-step", PromptName: "client-reply"})
	require.NotNil(t, result["kind"])
	h.fakeHost.mu.Lock()
	h.workflowReader = replyTestWorkflows{}
	h.fakeHost.mu.Unlock()
	require.NoError(t, p.connectionSvc.SetEnabled(context.Background(), "ws-1", false))
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("event-1")))
	require.NoError(t, p.connectionSvc.SetEnabled(context.Background(), "ws-1", true))
	h.fakeHost.mu.Lock()
	h.tasks["task-1"].WorkflowStepID = "different-step"
	h.fakeHost.mu.Unlock()
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("event-2")))
	h.mu.Lock()
	require.Empty(t, h.sends)
	h.mu.Unlock()
}

func TestReplyDraftingIsDisabledByDefaultAndRejectsForgedTaskScope(t *testing.T) {
	p, h, _ := newCommentFixture(t)
	settings := handle(t, p, "replies.settings.get", "ws-1", "", nil)
	require.Equal(t, "", settings["step_id"])
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("unconfigured-event")))
	h.mu.Lock()
	h.tasks["task-1"].WorkspaceID = "other-workspace"
	h.mu.Unlock()
	result := commentAction(t, p, "comments.get", "human", nil)
	require.NotNil(t, result["kind"])
}

func TestCompletedNonDraftReplyAllowsAnExplicitNewRequest(t *testing.T) {
	for _, response := range []string{"BLOCKED: deployment evidence is missing.", "```redmine-comment\nmalformed json\n```"} {
		t.Run(response, func(t *testing.T) {
			p, h, f := newAutomaticReplyFixture(t)
			require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("first-request")))
			finishAutomaticDraft(t, p, h, false)
			h.mu.Lock()
			h.messages[len(h.messages)-1].Content = response
			h.mu.Unlock()
			view := commentAction(t, p, "comments.get", "human", nil)
			require.Equal(t, true, view["can_request_draft"])
			require.Nil(t, view["proposal"])
			result := commentAction(t, p, "replies.request", "human", map[string]any{"request_id": "evidence-fixed"})
			require.Equal(t, "manual:evidence-fixed", result["id"])
			require.Equal(t, "sent", result["state"])
			h.mu.Lock()
			sends := len(h.sends)
			h.mu.Unlock()
			require.Equal(t, 2, sends)
			puts, _ := f.snapshot()
			require.Zero(t, puts)
		})
	}
}

func TestNewReplyWaitsForNativeCompletionEvenWithAValidProposal(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			p, h, _ := newAutomaticReplyFixture(t)
			require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("first-request")))
			finishAutomaticDraft(t, p, h, false)
			if pending {
				h.pending = true
			} else {
				h.sessionState = "RUNNING"
			}
			require.Equal(t, false, commentAction(t, p, "comments.get", "human", nil)["can_request_draft"])
			result := commentAction(t, p, "replies.request", "human", map[string]any{"request_id": "while-active"})
			require.Equal(t, "event:first-request", result["id"])
			h.pending, h.sessionState = false, "WAITING_FOR_INPUT"
			require.Equal(t, "event:first-request", commentAction(t, p, "replies.request", "human", map[string]any{"request_id": "while-active"})["id"])
			require.Equal(t, "manual:after-completion", commentAction(t, p, "replies.request", "human", map[string]any{"request_id": "after-completion"})["id"])
		})
	}
}
