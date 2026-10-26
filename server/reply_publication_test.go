package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

type automaticReplyHost struct {
	*replyMessageHost
	sessionState   string
	sessionProfile string
	pending        bool
	pendingErr     error
}

func (h *automaticReplyHost) Sessions() pluginsdk.SessionReader { return automaticSessions{host: h} }
func (h *automaticReplyHost) Interactions() pluginsdk.InteractionAccessor {
	return automaticInteractions{host: h}
}
func (h *automaticReplyHost) Tasks() pluginsdk.TaskReader {
	return automaticTasks{TaskReader: h.fakeHost.Tasks(), host: h}
}

type automaticSessions struct {
	pluginsdk.SessionReader
	host *automaticReplyHost
}

func (r automaticSessions) List(_ context.Context, filter pluginsdk.SessionFilter, _ pluginsdk.Page) ([]pluginsdk.Session, *pluginsdk.PageInfo, error) {
	if len(filter.TaskIDs) != 1 || filter.TaskIDs[0] != "task-1" || len(filter.WorkspaceIDs) != 1 || filter.WorkspaceIDs[0] != "ws-1" {
		return nil, nil, fmt.Errorf("unexpected session scope")
	}
	return []pluginsdk.Session{{ID: "draft-session", TaskID: "task-1", AgentProfileID: r.host.sessionProfile, State: r.host.sessionState, StartedAt: "2026-10-26T12:00:00Z"}}, nil, nil
}

type automaticInteractions struct {
	pluginsdk.InteractionAccessor
	host *automaticReplyHost
}

func (r automaticInteractions) ListPending(_ context.Context, filter pluginsdk.InteractionFilter, _ pluginsdk.Page) ([]pluginsdk.Interaction, *pluginsdk.PageInfo, error) {
	if len(filter.TaskIDs) != 1 || filter.TaskIDs[0] != "task-1" {
		return nil, nil, fmt.Errorf("unexpected interaction scope")
	}
	if r.host.pendingErr != nil {
		return nil, nil, r.host.pendingErr
	}
	if r.host.pending {
		return []pluginsdk.Interaction{{ID: "human-question"}}, nil, nil
	}
	return nil, nil, nil
}

type automaticTasks struct {
	pluginsdk.TaskReader
	host *automaticReplyHost
}

func (r automaticTasks) List(ctx context.Context, filter pluginsdk.TaskFilter, _ pluginsdk.Page) ([]pluginsdk.Task, *pluginsdk.PageInfo, error) {
	if len(filter.WorkspaceIDs) != 1 || filter.WorkspaceIDs[0] != "ws-1" || len(filter.WorkflowIDs) != 1 || filter.WorkflowIDs[0] != "wf-1" {
		return nil, nil, fmt.Errorf("unexpected task scope")
	}
	task, err := r.TaskReader.Get(ctx, "task-1")
	if err != nil {
		return nil, nil, err
	}
	return []pluginsdk.Task{*task}, nil, nil
}

func newAutomaticReplyFixture(t *testing.T) (*redminePlugin, *automaticReplyHost, *commentFixture) {
	t.Helper()
	p, base, fixture := newReplyFixture(t)
	h := &automaticReplyHost{replyMessageHost: base, sessionState: "WAITING_FOR_INPUT", sessionProfile: "read-only-profile"}
	p.UnimplementedPlugin.SetHost(h)
	h.fakeHost.mu.Lock()
	h.fakeHost.tasks["task-1"].State = "WAITING_FOR_INPUT"
	h.fakeHost.mu.Unlock()
	return p, h, fixture
}

func setAutomaticMode(t *testing.T, p *redminePlugin, enabled bool) map[string]any {
	t.Helper()
	return commentAction(t, p, "replies.settings.save", "operator", replySettings{WorkflowID: "wf-1", StepID: "reply-step", PromptName: "client-reply", AutoPublish: enabled, PolicyID: "forged", AuthorizedBy: "forged"})
}

func finishAutomaticDraft(t *testing.T, p *redminePlugin, h *automaticReplyHost, private bool) {
	t.Helper()
	value, found, err := p.Host().GetState(context.Background(), "task", "task-1", replyLatestKey)
	require.NoError(t, err)
	require.True(t, found)
	id := value["id"].(string)
	hash := sha256.Sum256([]byte(id))
	body, err := json.Marshal(commentProposal{IssueID: 42, RequestID: hex.EncodeToString(hash[:]), Notes: "Completed client reply.", PrivateNotes: &private})
	require.NoError(t, err)
	h.mu.Lock()
	h.messages = append(h.messages, pluginsdk.Message{ID: "final-reply", SessionID: "draft-session", TaskID: "task-1", TurnID: "completed-turn", Type: "message", AuthorType: "agent", Content: "```redmine-comment\n" + string(body) + "\n```", CreatedAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)})
	h.mu.Unlock()
}

func TestAutomaticPublicationIsOffByDefaultAndDoesNotDrainManualDrafts(t *testing.T) {
	p, h, f := newAutomaticReplyFixture(t)
	settings := handle(t, p, "replies.settings.get", "ws-1", "", nil)
	require.Equal(t, false, settings["auto_publish"])
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("manual-request")))
	finishAutomaticDraft(t, p, h, false)
	require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
	setAutomaticMode(t, p, true)
	require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
	puts, _ := f.snapshot()
	require.Zero(t, puts)
}

func TestAutomaticPolicyUsesAuthenticatedOperatorAndRotatesAfterDisabling(t *testing.T) {
	p, _, _ := newAutomaticReplyFixture(t)
	first := setAutomaticMode(t, p, true)
	require.NotNil(t, first["policy_id"])
	require.NotEqual(t, "forged", first["policy_id"])
	require.Equal(t, "operator", first["authorized_by"])
	require.Equal(t, first["policy_id"], setAutomaticMode(t, p, true)["policy_id"])
	require.Nil(t, setAutomaticMode(t, p, false)["policy_id"])
	require.NotEqual(t, first["policy_id"], setAutomaticMode(t, p, true)["policy_id"])
	require.NotNil(t, commentAction(t, p, "replies.settings.save", "", replySettings{WorkflowID: "wf-1", StepID: "reply-step", PromptName: "client-reply", AutoPublish: true})["kind"])
	require.NotNil(t, commentAction(t, p, "replies.settings.save", "operator", replySettings{AutoPublish: true})["kind"])
}

func TestAutomaticRepliesPublishOnceWithPolicyReceipt(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprint(private), func(t *testing.T) {
			p, h, f := newAutomaticReplyFixture(t)
			settings := setAutomaticMode(t, p, true)
			require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("auto-request")))
			finishAutomaticDraft(t, p, h, private)
			for range 2 {
				require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
			}
			restarted := &redminePlugin{connectionSvc: p.connectionSvc, tasklinkSvc: p.tasklinkSvc, fieldmappingSvc: p.fieldmappingSvc, projectsSvc: p.projectsSvc}
			restarted.UnimplementedPlugin.SetHost(h)
			require.NoError(t, restarted.pollReplies(context.Background(), "ws-1"))
			puts, payload := f.snapshot()
			require.Equal(t, 1, puts)
			require.Equal(t, map[string]any{"notes": "Completed client reply.", "private_notes": private}, payload)
			draft, err := p.currentComment(context.Background(), "task-1")
			require.NoError(t, err)
			require.Equal(t, "published", draft.State)
			require.Equal(t, "automatic-policy:"+settings["policy_id"].(string), draft.ApprovedBy)
			require.Equal(t, "operator", draft.ActorID)
			require.Equal(t, "final-reply", draft.SourceMessageID)
			require.Equal(t, "event:auto-request", draft.SourceRequestID)
		})
	}
}

func TestAutomaticRepliesWaitForCompletionAndHumanQuestions(t *testing.T) {
	p, h, f := newAutomaticReplyFixture(t)
	setAutomaticMode(t, p, true)
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("auto-request")))
	finishAutomaticDraft(t, p, h, false)
	h.sessionState = "RUNNING"
	require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
	h.sessionState, h.pending = "WAITING_FOR_INPUT", true
	require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
	puts, _ := f.snapshot()
	require.Zero(t, puts)
	h.pending = false
	require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
	puts, _ = f.snapshot()
	require.Equal(t, 1, puts)
}

func TestAutomaticRepliesRequireCurrentAuthorityAndExactFinalDraft(t *testing.T) {
	changes := map[string]func(*testing.T, *redminePlugin, *automaticReplyHost, *commentFixture){
		"disabled": func(t *testing.T, p *redminePlugin, _ *automaticReplyHost, _ *commentFixture) {
			setAutomaticMode(t, p, false)
		},
		"disabled-and-reenabled": func(t *testing.T, p *redminePlugin, _ *automaticReplyHost, _ *commentFixture) {
			setAutomaticMode(t, p, false)
			setAutomaticMode(t, p, true)
		},
		"stale-issue": func(_ *testing.T, _ *redminePlugin, _ *automaticReplyHost, f *commentFixture) {
			f.mu.Lock()
			f.updated = "2026-10-26T11:00:00Z"
			f.mu.Unlock()
		},
		"profile-changed": func(_ *testing.T, _ *redminePlugin, h *automaticReplyHost, _ *commentFixture) {
			h.sessionProfile = "another-profile"
		},
		"session-failed": func(_ *testing.T, _ *redminePlugin, h *automaticReplyHost, _ *commentFixture) {
			h.sessionState = "FAILED"
		},
		"missing-interaction-access": func(_ *testing.T, _ *redminePlugin, h *automaticReplyHost, _ *commentFixture) {
			h.pendingErr = fmt.Errorf("permission denied: interactions")
		},
		"wrong-request": func(_ *testing.T, _ *redminePlugin, h *automaticReplyHost, _ *commentFixture) {
			h.messages[len(h.messages)-1].Content = strings.Replace(h.messages[len(h.messages)-1].Content, "request_id", "unknown_id", 1)
		},
		"wrong-session": func(_ *testing.T, _ *redminePlugin, h *automaticReplyHost, _ *commentFixture) {
			h.messages[len(h.messages)-1].SessionID = "another-session"
		},
		"missing-turn": func(_ *testing.T, _ *redminePlugin, h *automaticReplyHost, _ *commentFixture) {
			h.messages[len(h.messages)-1].TurnID = ""
		},
		"manual-preview": func(t *testing.T, p *redminePlugin, _ *automaticReplyHost, _ *commentFixture) {
			prepareComment(t, p, false)
		},
		"invalid-publication-state": func(t *testing.T, p *redminePlugin, _ *automaticReplyHost, _ *commentFixture) {
			value, _, err := p.Host().GetState(context.Background(), "task", "task-1", replyLatestKey)
			require.NoError(t, err)
			value["publication_state"] = "unknown"
			require.NoError(t, p.Host().SetState(context.Background(), "task", "task-1", replyLatestKey, value))
		},
		"connection-changed": func(t *testing.T, p *redminePlugin, _ *automaticReplyHost, _ *commentFixture) {
			conn, _, err := p.connectionSvc.Get(context.Background(), "ws-1")
			require.NoError(t, err)
			handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": conn.BaseURL, "api_key": "changed-fixture-only"})
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			p, h, f := newAutomaticReplyFixture(t)
			setAutomaticMode(t, p, true)
			require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("auto-request")))
			finishAutomaticDraft(t, p, h, false)
			change(t, p, h, f)
			for range 2 {
				require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
			}
			puts, _ := f.snapshot()
			require.Zero(t, puts)
			if name != "disabled" {
				value, _, err := p.Host().GetState(context.Background(), "task", "task-1", replyLatestKey)
				require.NoError(t, err)
				require.Equal(t, "needs_review", value["publication_state"])
				require.NotEmpty(t, value["publication_error"])
			}
		})
	}
}

func TestAutomaticRepliesNeverRetryAnUncertainAppend(t *testing.T) {
	for _, appended := range []bool{false, true} {
		t.Run(fmt.Sprint(appended), func(t *testing.T) {
			p, h, f := newAutomaticReplyFixture(t)
			setAutomaticMode(t, p, true)
			require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("auto-request")))
			finishAutomaticDraft(t, p, h, false)
			f.mu.Lock()
			f.status, f.appendComment = 500, appended
			f.mu.Unlock()
			for range 3 {
				require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
			}
			restarted := &redminePlugin{connectionSvc: p.connectionSvc, tasklinkSvc: p.tasklinkSvc, fieldmappingSvc: p.fieldmappingSvc, projectsSvc: p.projectsSvc}
			restarted.UnimplementedPlugin.SetHost(h)
			require.NoError(t, restarted.pollReplies(context.Background(), "ws-1"))
			puts, _ := f.snapshot()
			require.Equal(t, 1, puts)
			draft, err := p.currentComment(context.Background(), "task-1")
			require.NoError(t, err)
			if appended {
				require.Equal(t, "published", draft.State)
			} else {
				require.Equal(t, "unverified", draft.State)
			}
		})
	}
}

func TestEditingAnOlderPreviewDoesNotRaceAutomaticPublication(t *testing.T) {
	p, h, f := newAutomaticReplyFixture(t)
	draft := prepareComment(t, p, false)
	setAutomaticMode(t, p, true)
	require.NoError(t, p.OnEvent(context.Background(), movedReplyEvent("auto-request")))
	finishAutomaticDraft(t, p, h, false)
	commentAction(t, p, "comments.discard", "human", map[string]any{"draft_id": draft["id"]})
	require.NoError(t, p.pollReplies(context.Background(), "ws-1"))
	puts, _ := f.snapshot()
	require.Zero(t, puts)
	value, _, err := p.Host().GetState(context.Background(), "task", "task-1", replyLatestKey)
	require.NoError(t, err)
	require.Equal(t, "needs_review", value["publication_state"])
}
