package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"kandev-plugin-redmine/internal/issues"
)

const replySettingsKey = "reply.settings"
const replyLatestKey = "reply.latest"

//go:embed reply-draft.md
var replyDraftInstructions string

type replySettings struct {
	WorkflowID string `json:"workflow_id"`
	StepID     string `json:"step_id"`
	PromptName string `json:"prompt_name"`
}

type replyRequest struct {
	ID          string `json:"id"`
	IssueID     int    `json:"issue_id"`
	State       string `json:"state"`
	RequestedAt string `json:"requested_at"`
	SessionID   string `json:"session_id,omitempty"`
}

func (p *redminePlugin) replySettings(ctx context.Context, workspaceID string) (replySettings, error) {
	value, _, err := p.Host().GetState(ctx, "workspace", workspaceID, replySettingsKey)
	if err != nil {
		return replySettings{}, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return replySettings{}, err
	}
	var settings replySettings
	err = json.Unmarshal(encoded, &settings)
	return settings, err
}

func (p *redminePlugin) handleReplySettings(ctx context.Context, req *pluginsdk.PluginActionRequest, save bool) (*pluginsdk.PluginActionResponse, error) {
	if !save {
		settings, err := p.replySettings(ctx, req.Context.WorkspaceID)
		if err != nil {
			return nil, err
		}
		return jsonResponse(settings)
	}
	settings, err := decodeBody[replySettings](req)
	if err != nil {
		return nil, err
	}
	if settings.StepID != "" {
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,79}$`).MatchString(settings.PromptName) {
			return classifiedErrorResponse(fmt.Errorf("redmine: choose a saved prompt name using letters, numbers, dots, underscores or hyphens"))
		}
		if err := p.validateReplyStep(ctx, req.Context.WorkspaceID, settings); err != nil {
			return classifiedErrorResponse(err)
		}
	}
	encoded, _ := json.Marshal(settings)
	var value map[string]any
	_ = json.Unmarshal(encoded, &value)
	if err := p.Host().SetState(ctx, "workspace", req.Context.WorkspaceID, replySettingsKey, value); err != nil {
		return nil, err
	}
	return jsonResponse(settings)
}

func (p *redminePlugin) validateReplyStep(ctx context.Context, workspaceID string, settings replySettings) error {
	mapping, found, err := p.fieldmappingSvc.Get(ctx, workspaceID)
	if err != nil {
		return err
	}
	if found && mapping.WorkflowID == settings.WorkflowID {
		if _, mapped := mapping.StatusForWorkflowStep(settings.StepID); mapped {
			return fmt.Errorf("redmine: do not map the human-review drafting step to an automatic Redmine status")
		}
	}
	workflows, err := listWorkspaceWorkflows(ctx, p.Host().Workflows(), workspaceID)
	if err != nil {
		return err
	}
	for _, workflow := range workflows {
		if workflow.ID != settings.WorkflowID {
			continue
		}
		steps, err := p.Host().Workflows().ListSteps(ctx, workflow.ID)
		if err != nil {
			return err
		}
		for _, step := range steps {
			if step.ID != settings.StepID {
				continue
			}
			if step.AgentProfileID == "" {
				return fmt.Errorf("redmine: the drafting step needs a fixed read-only agent profile")
			}
			for _, action := range step.OnEnterActionTypes {
				if action == "auto_start_agent" {
					return fmt.Errorf("redmine: disable native auto-start on the drafting step; the plugin sends one live issue context")
				}
			}
			return nil
		}
	}
	return fmt.Errorf("redmine: drafting step is unavailable in this workspace")
}

func (p *redminePlugin) requestReply(ctx context.Context, workspaceID, taskID, requestID string) (*replyRequest, error) {
	p.commentMu.Lock()
	defer p.commentMu.Unlock()
	settings, err := p.replySettings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if settings.StepID == "" || strings.TrimSpace(requestID) == "" || len(requestID) > 200 {
		return nil, fmt.Errorf("redmine: configure a communication step before requesting a draft")
	}
	if err := p.validateReplyStep(ctx, workspaceID, settings); err != nil {
		return nil, err
	}
	task, err := p.Host().Tasks().Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil || task.WorkspaceID != workspaceID || task.WorkflowID != settings.WorkflowID || task.WorkflowStepID != settings.StepID || task.ArchivedAt != nil {
		return nil, fmt.Errorf("redmine: move this task to the configured communication step first")
	}
	if task.QueuedAt != nil || task.QueuedForStepID != "" {
		return nil, fmt.Errorf("redmine: this task is awaiting workflow admission; request its draft after admission")
	}
	_, _, issue, err := p.linkedCommentContext(ctx, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(requestID))
	key := "reply.request." + hex.EncodeToString(hash[:])
	previous, found, err := p.Host().GetState(ctx, "task", taskID, key)
	if err != nil {
		return nil, err
	}
	if found {
		raw, _ := json.Marshal(previous)
		var result replyRequest
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		if result.ID == "" || result.IssueID != issue.ID || (result.State != "sending" && result.State != "uncertain" && result.State != "sent" && result.State != "denied") {
			return nil, fmt.Errorf("redmine: stored draft dispatch does not match this issue; inspect its native task")
		}
		return &result, nil
	}
	last, found, err := p.Host().GetState(ctx, "task", taskID, replyLatestKey)
	if err != nil {
		return nil, err
	}
	if found && last["state"] != "sending" && last["state"] != "uncertain" && last["state"] != "sent" && last["state"] != "denied" {
		return nil, fmt.Errorf("redmine: stored draft dispatch is invalid; inspect its native task")
	}
	if found && (last["state"] == "sending" || last["state"] == "uncertain") {
		return nil, fmt.Errorf("redmine: an earlier draft dispatch is uncertain; inspect its native task before requesting another")
	}
	if found && last["state"] == "sent" {
		message, readErr := p.latestTaskMessage(ctx, taskID)
		requestedValue, _ := last["requested_at"].(string)
		requestedAt, _ := time.Parse(time.RFC3339Nano, requestedValue)
		var messageAt time.Time
		if message != nil {
			messageAt, _ = time.Parse(time.RFC3339Nano, message.CreatedAt)
		}
		if readErr != nil || message == nil || message.AuthorType != "agent" || !messageAt.After(requestedAt) || parseCommentProposal(message.Content, issue.ID) == nil {
			// Coalesce a second event/click while its original request is still
			// meaningful work; retain the alias so a replay cannot launch later.
			if err := p.Host().SetState(ctx, "task", taskID, key, last); err != nil {
				return nil, err
			}
			encoded, _ := json.Marshal(last)
			var previous replyRequest
			_ = json.Unmarshal(encoded, &previous)
			return &previous, nil
		}
	}
	var publicJournals []issues.Journal
	for _, journal := range issue.Journals {
		if !journal.PrivateNotes {
			publicJournals = append(publicJournals, journal)
		}
	}
	evidence, err := json.Marshal(map[string]any{"issue_id": issue.ID, "issue_url": issue.URL, "project_id": issue.ProjectID, "subject": issue.Subject, "description": issue.Description, "updated_on": issue.UpdatedOn, "public_journals": publicJournals, "private_notes_omitted": true})
	if err != nil {
		return nil, err
	}
	if len(evidence) > 128*1024 {
		return nil, fmt.Errorf("redmine: issue context exceeds 128 KiB; prepare focused input manually")
	}
	prompt := fmt.Sprintf(replyDraftInstructions, settings.PromptName, issue.ID, hex.EncodeToString(hash[:]), evidence)
	result := &replyRequest{ID: requestID, IssueID: issue.ID, State: "sending", RequestedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	save := func() error {
		raw, _ := json.Marshal(result)
		var value map[string]any
		_ = json.Unmarshal(raw, &value)
		if err := p.Host().SetState(ctx, "task", taskID, key, value); err != nil {
			return err
		}
		return p.Host().SetState(ctx, "task", taskID, replyLatestKey, value)
	}
	if err := save(); err != nil {
		return nil, err
	}
	dispatch, sendErr := p.Host().Messages().Send(ctx, taskID, "", prompt)
	result.State = "uncertain"
	if status.Code(sendErr) == codes.PermissionDenied || status.Code(sendErr) == codes.Unauthenticated {
		result.State = "denied"
	}
	if sendErr == nil && dispatch != nil && dispatch.SessionID != "" && (dispatch.Status == "queued" || dispatch.Status == "sent" || dispatch.Status == "started") {
		result.State = "sent"
		result.SessionID = dispatch.SessionID
	}
	if err := save(); err != nil {
		return nil, err
	}
	return result, nil
}

func (p *redminePlugin) handleReplyRequest(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if req.Context.ActorID == "" {
		return classifiedErrorResponse(fmt.Errorf("redmine: authenticated operator required"))
	}
	body, err := decodeBody[struct {
		RequestID string `json:"request_id"`
	}](req)
	if err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`).MatchString(body.RequestID) {
		return classifiedErrorResponse(fmt.Errorf("redmine: request_id must be a stable nonempty identifier"))
	}
	result, err := p.requestReply(ctx, req.Context.WorkspaceID, req.Context.TaskID, "manual:"+body.RequestID)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	return jsonResponse(result)
}

func (p *redminePlugin) autoDraftReply(ctx context.Context, e *pluginsdk.Event, workspaceID, taskID, stepID string) error {
	settings, err := p.replySettings(ctx, workspaceID)
	if err != nil {
		return err
	}
	if settings.StepID == "" || stepID != settings.StepID {
		return nil
	}
	enabled, err := p.connectionSvc.GetEnabled(ctx, workspaceID)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	task, err := p.Host().Tasks().Get(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil || task.WorkflowStepID != stepID {
		return nil
	}
	if e.EventID == "" {
		return fmt.Errorf("redmine: draft event has no stable id")
	}
	_, err = p.requestReply(ctx, workspaceID, taskID, "event:"+e.EventID)
	return err
}
