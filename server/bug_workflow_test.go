package main

import (
	"context"
	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
	"kandev-plugin-redmine/internal/fieldmapping"
	"kandev-plugin-redmine/internal/issues"
	"kandev-plugin-redmine/internal/redmineclient"
	"kandev-plugin-redmine/internal/watch"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type multiReplyWorkflows struct{ autoStartEntry bool }

func (multiReplyWorkflows) List(context.Context, string, pluginsdk.Page) ([]pluginsdk.Workflow, *pluginsdk.PageInfo, error) {
	return []pluginsdk.Workflow{{ID: "wf-1"}, {ID: "wf-bug"}}, nil, nil
}
func (r multiReplyWorkflows) ListSteps(_ context.Context, workflowID string) ([]pluginsdk.WorkflowStep, error) {
	if workflowID == "wf-1" {
		return replyTestWorkflows{}.ListSteps(context.Background(), workflowID)
	}
	entry := pluginsdk.WorkflowStep{ID: "diagnosis", AgentProfileID: "read-only-profile"}
	if r.autoStartEntry {
		entry.OnEnterActionTypes = []string{"auto_start_agent"}
	}
	return []pluginsdk.WorkflowStep{entry, {ID: "first-reply", AgentProfileID: "read-only-profile"}, {ID: "final-reply", AgentProfileID: "read-only-profile"}}, nil
}

func multiReplyConfig() replyConfiguration {
	return replyConfiguration{replySettings: replySettings{WorkflowID: "wf-1", StepID: "reply-step", PromptName: "client-reply"}, AdditionalSteps: []replySettings{
		{WorkflowID: "wf-bug", StepID: "first-reply", PromptName: "diagnosis-reply"},
		{WorkflowID: "wf-bug", StepID: "final-reply", PromptName: "fix-ready-reply"},
	}}
}

func TestTwoCommunicationStagesUseTheirOwnPromptWithoutPublishing(t *testing.T) {
	p, h, f := newAutomaticReplyFixture(t)
	h.workflowReader = multiReplyWorkflows{}
	result := commentAction(t, p, "replies.settings.save", "operator", multiReplyConfig())
	require.Nil(t, result["kind"])
	for i, step := range []string{"first-reply", "final-reply"} {
		h.tasks["task-1"].WorkflowID, h.tasks["task-1"].WorkflowStepID = "wf-bug", step
		e := movedReplyEvent("stage:" + step)
		e.Payload["to_step_id"] = step
		require.NoError(t, p.OnEvent(context.Background(), e))
		require.NoError(t, p.OnEvent(context.Background(), e), "repeated entry event must not duplicate a draft")
		require.Contains(t, h.sends[i], "@"+multiReplyConfig().AdditionalSteps[i].PromptName)
		finishAutomaticDraft(t, p, h, false)
		require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
		if i == 0 {
			h.messages = append(h.messages, pluginsdk.Message{ID: "implementation-output", SessionID: "implementation-session", TaskID: "task-1", TurnID: "implementation-turn", AuthorType: "agent", Type: "message", Content: "Implementation completed; browser acceptance passed.", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
			h.extraSessions = []pluginsdk.Session{{ID: "implementation-session", TaskID: "task-1", AgentProfileID: "implementation-profile", State: "COMPLETED", StartedAt: "2026-10-26T13:00:00Z"}, {ID: "fix-ready-session", TaskID: "task-1", AgentProfileID: "read-only-profile", State: "CREATED", StartedAt: "2026-10-26T14:00:00Z"}}
		}
	}
	require.Equal(t, []string{"draft-session", "fix-ready-session"}, h.sessionIDs)
	puts, _ := f.snapshot()
	require.Zero(t, puts)
	settings := handle(t, p, "replies.settings.get", "ws-1", "", nil)
	require.Equal(t, "reply-step", settings["step_id"], "normal workflow must remain configured")
	require.Len(t, settings["additional_steps"], 2)
}

func TestCommunicationPoliciesRemainIndependentAndRejectDuplicateSteps(t *testing.T) {
	p, h, _ := newAutomaticReplyFixture(t)
	h.workflowReader = multiReplyWorkflows{}
	config := multiReplyConfig()
	config.AutoPublish = true
	config.AdditionalSteps[1].AutoPublish = true
	first := commentAction(t, p, "replies.settings.save", "operator", config)
	require.Nil(t, first["kind"])
	old, err := p.replySettings(context.Background(), "ws-1")
	require.NoError(t, err)
	require.NotEmpty(t, old.PolicyID)
	require.Empty(t, old.AdditionalSteps[0].PolicyID)
	require.NotEmpty(t, old.AdditionalSteps[1].PolicyID)
	require.NotEqual(t, old.PolicyID, old.AdditionalSteps[1].PolicyID)
	config.AdditionalSteps[0].PromptName = "new-first-prompt"
	require.Nil(t, commentAction(t, p, "replies.settings.save", "operator", config)["kind"])
	current, err := p.replySettings(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Equal(t, old.PolicyID, current.PolicyID)
	require.Equal(t, old.AdditionalSteps[1].PolicyID, current.AdditionalSteps[1].PolicyID)
	config.AdditionalSteps[1].StepID = "first-reply"
	require.NotNil(t, commentAction(t, p, "replies.settings.save", "operator", config)["kind"])
	unchanged, err := p.replySettings(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Equal(t, current, unchanged)
}

func TestAutomaticReplyCannotPublishEarlierStagesDraftInLaterStage(t *testing.T) {
	p, h, f := newAutomaticReplyFixture(t)
	h.workflowReader = multiReplyWorkflows{}
	config := multiReplyConfig()
	config.AdditionalSteps[0].AutoPublish, config.AdditionalSteps[1].AutoPublish = true, true
	require.Nil(t, commentAction(t, p, "replies.settings.save", "operator", config)["kind"])
	h.tasks["task-1"].WorkflowID, h.tasks["task-1"].WorkflowStepID = "wf-bug", "first-reply"
	_, err := p.requestReply(context.Background(), "ws-1", "task-1", "first")
	require.NoError(t, err)
	finishAutomaticDraft(t, p, h, false)
	h.tasks["task-1"].WorkflowStepID = "final-reply"
	require.NoError(t, p.publishAutomaticReply(context.Background(), "ws-1", "task-1"))
	puts, _ := f.snapshot()
	require.Zero(t, puts)
	last, _, err := h.GetState(context.Background(), "task", "task-1", replyLatestKey)
	require.NoError(t, err)
	require.Equal(t, "needs_review", last["publication_state"])
}

func TestAdditionalStageCanPublishOnlyWhenItsOwnModeIsEnabled(t *testing.T) {
	p, h, f := newAutomaticReplyFixture(t)
	h.workflowReader = multiReplyWorkflows{}
	config := multiReplyConfig()
	config.AdditionalSteps[1].AutoPublish = true
	require.Nil(t, commentAction(t, p, "replies.settings.save", "operator", config)["kind"])
	h.tasks["task-1"].WorkflowID, h.tasks["task-1"].WorkflowStepID = "wf-bug", "final-reply"
	_, err := p.requestReply(context.Background(), "ws-1", "task-1", "final")
	require.NoError(t, err)
	finishAutomaticDraft(t, p, h, false)
	require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
	require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
	puts, _ := f.snapshot()
	require.Equal(t, 1, puts)
}

func TestTrackerRouteValidationUsesLiveTrackerAndScopedManualEntry(t *testing.T) {
	p, h := newTestPlugin(t)
	h.workflowReader = multiReplyWorkflows{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/trackers.json", r.URL.Path)
		_, _ = w.Write([]byte(`{"trackers":[{"id":8,"name":"Defect"}]}`))
	}))
	defer srv.Close()
	client := redmineclient.New(srv.URL, "key", srv.Client())
	valid := fieldmapping.TrackerRoute{RedmineTrackerID: 8, WorkflowID: "wf-bug", WorkflowStepID: "diagnosis"}
	require.NoError(t, p.validateTrackerRoutes(context.Background(), "ws-1", client, []fieldmapping.TrackerRoute{valid}))
	for _, invalid := range []fieldmapping.TrackerRoute{
		{RedmineTrackerID: 1, WorkflowID: "wf-bug", WorkflowStepID: "diagnosis"},
		{RedmineTrackerID: 8, WorkflowID: "foreign", WorkflowStepID: "diagnosis"},
		{RedmineTrackerID: 8, WorkflowID: "wf-bug", WorkflowStepID: "absent"},
	} {
		require.Error(t, p.validateTrackerRoutes(context.Background(), "ws-1", client, []fieldmapping.TrackerRoute{invalid}))
	}
	require.Error(t, p.validateTrackerRoutes(context.Background(), "ws-1", client, []fieldmapping.TrackerRoute{valid, valid}))
	h.workflowReader = multiReplyWorkflows{autoStartEntry: true}
	require.ErrorContains(t, p.validateTrackerRoutes(context.Background(), "ws-1", client, []fieldmapping.TrackerRoute{valid}), "must not auto-start")
}

func TestExistingWatchUsesCurrentTrackerRoutingOnEachPoll(t *testing.T) {
	p, h := newTestPlugin(t)
	_, err := p.watchSvc.CreateWatch(context.Background(), watch.Watch{WorkspaceID: "ws-1", ProjectID: 1, WorkflowID: "wf-1", WorkflowStepID: "backlog", Enabled: true})
	require.NoError(t, err)
	mapping := fieldmapping.Mapping{WorkflowID: "wf-1", TrackerRoutes: []fieldmapping.TrackerRoute{{RedmineTrackerID: 8, WorkflowID: "wf-bug", WorkflowStepID: "diagnosis"}}}
	require.NoError(t, p.fieldmappingSvc.Save(context.Background(), "ws-1", mapping))
	srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, _ *http.Request) {
		_, _ = out.Write([]byte(`{"issues":[{"id":42,"tracker":{"id":8}}],"total_count":1}`))
	}))
	defer srv.Close()
	require.NoError(t, p.pollWatches(context.Background(), "ws-1", issues.New(redmineclient.New(srv.URL, "key", srv.Client()))))
	require.Len(t, h.tasks, 1)
	for _, task := range h.tasks {
		require.Equal(t, "wf-bug", task.WorkflowID)
		require.Equal(t, "diagnosis", task.WorkflowStepID)
	}
	watches, err := p.watchSvc.ListWatches(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Equal(t, "wf-1", watches[0].WorkflowID, "route is resolved dynamically, not persisted onto the watch")
}
