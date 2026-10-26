package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// Reuse the connector's poll loop; the agent never receives publishing credentials.
func (p *redminePlugin) pollReplies(ctx context.Context, workspaceID string) error {
	settings, err := p.replySettings(ctx, workspaceID)
	if err != nil || !settings.AutoPublish || settings.StepID == "" {
		return err
	}
	page := pluginsdk.Page{Limit: 100}
	var failures []error
	for n := 0; n < 20; n++ {
		tasks, info, err := p.Host().Tasks().List(ctx, pluginsdk.TaskFilter{WorkspaceIDs: []string{workspaceID}, WorkflowIDs: []string{settings.WorkflowID}}, page)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if task.WorkspaceID == workspaceID && task.WorkflowID == settings.WorkflowID && task.WorkflowStepID == settings.StepID {
				if err := p.publishAutomaticReply(ctx, workspaceID, task.ID); err != nil {
					failures = append(failures, fmt.Errorf("task %s: %w", task.ID, err))
				}
			}
		}
		if info == nil || !info.HasMore {
			return errors.Join(failures...)
		}
		if info.NextCursor == "" || info.NextCursor == page.Cursor {
			return fmt.Errorf("redmine: invalid automatic reply task cursor")
		}
		page.Cursor = info.NextCursor
	}
	return fmt.Errorf("redmine: automatic reply task scan exceeds 2000 tasks")
}

func (p *redminePlugin) publishAutomaticReply(ctx context.Context, workspaceID, taskID string) error {
	p.commentMu.Lock()
	defer p.commentMu.Unlock()
	settings, err := p.replySettings(ctx, workspaceID)
	if err != nil || !settings.AutoPublish || settings.PolicyID == "" || settings.AuthorizedBy == "" {
		return err
	}
	value, found, err := p.Host().GetState(ctx, "task", taskID, replyLatestKey)
	if err != nil || !found {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var request replyRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	// Opting in never drains old drafts, or promotes a manual dispatch.
	if request.PolicyID == "" || request.State != "sent" || request.PublicationState == "published" || request.PublicationState == "needs_review" {
		return nil
	}
	if request.ID == "" || request.SessionID == "" {
		return fmt.Errorf("redmine: invalid automatic reply dispatch")
	}
	hold := func(reason string) error {
		request.PublicationState, request.PublicationError = "needs_review", reason
		return p.saveReplyRequest(ctx, taskID, &request)
	}
	if request.PublicationState != "waiting" && request.PublicationState != "unverified" && request.PublicationState != "rejected" && request.PublicationState != "cancelled" {
		return hold("The automatic publication state is invalid; inspect the native task.")
	}
	if request.PolicyID != settings.PolicyID {
		return hold("Publishing mode or configuration changed. Prepare a new draft or review this one manually.")
	}
	task, err := p.Host().Tasks().Get(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil || task.WorkspaceID != workspaceID || task.WorkflowID != settings.WorkflowID || task.WorkflowStepID != settings.StepID || task.ArchivedAt != nil || task.QueuedAt != nil || task.QueuedForStepID != "" {
		return hold("The task left its configured communication step or is unavailable.")
	}
	client, conn, issue, err := p.linkedCommentContext(ctx, workspaceID, taskID)
	if err != nil {
		return hold(err.Error())
	}
	if request.IssueID != issue.ID || request.ConnectionRevision != conn.Revision {
		return hold("The linked issue or connection changed.")
	}
	current, err := p.currentComment(ctx, taskID)
	if err != nil {
		return err
	}
	var draft *commentDraft
	if request.CommentID != "" {
		draft, err = p.loadComment(ctx, taskID, request.CommentID)
		if err != nil {
			return err
		}
	} else if current != nil && current.SourceRequestID == request.ID {
		draft = current
		request.CommentID = current.ID
		if err := p.saveReplyRequest(ctx, taskID, &request); err != nil {
			return err
		}
	}
	if draft != nil {
		if draft.WorkspaceID != workspaceID || draft.SourceRequestID != request.ID || draft.PolicyID != settings.PolicyID || draft.IssueID != issue.ID || draft.ConnectionRevision != conn.Revision {
			return hold("The publication receipt no longer matches this request.")
		}
		// Recovery of an attempted append only reads journals, even after a restart.
		if draft.State != "prepared" {
			result, err := p.publishComment(ctx, client, issue, draft, false, "")
			if err != nil {
				return err
			}
			request.PublicationState = result.State
			return p.saveReplyRequest(ctx, taskID, &request)
		}
	}
	if request.ContextHash == "" || request.ContextHash != commentContextHash(issue) {
		return hold("Redmine changed after drafting started. Review the current issue before publishing.")
	}
	if current != nil && (draft == nil || current.ID != draft.ID) {
		requestedAt, timeErr := time.Parse(time.RFC3339Nano, request.RequestedAt)
		previewTime := current.PreparedAt
		if current.CancelledAt != "" {
			previewTime = current.CancelledAt
		}
		preparedAt, previewErr := time.Parse(time.RFC3339Nano, previewTime)
		replaceable := current.State == "published" || current.State == "rejected" || current.State == "cancelled"
		if !replaceable || timeErr != nil || previewErr != nil || !preparedAt.Before(requestedAt) {
			return hold("An existing preview or publication requires human review; automatic publishing will not replace it.")
		}
	}
	profileID, err := p.validateReplyStep(ctx, workspaceID, settings)
	if err != nil {
		return hold(err.Error())
	}
	if request.ProfileID != profileID {
		return hold("The communication agent profile changed.")
	}
	if task.State != "WAITING_FOR_INPUT" && task.State != "COMPLETED" && task.State != "IDLE" {
		return nil
	}
	message, err := p.completedReplyMessage(ctx, workspaceID, taskID, request)
	if err != nil {
		return hold(err.Error())
	}
	if message == nil {
		return nil
	}
	proposal := parseCommentProposal(message.Content, issue.ID)
	hash := sha256.Sum256([]byte(request.ID))
	if proposal == nil || proposal.RequestID != hex.EncodeToString(hash[:]) {
		return hold("The final reply cannot be verified against this draft request. Review it manually.")
	}
	if draft == nil {
		draft, err = p.prepareComment(ctx, client, conn, issue, &commentDraft{WorkspaceID: workspaceID, TaskID: taskID, Notes: proposal.Notes, PrivateNotes: *proposal.PrivateNotes, ActorID: settings.AuthorizedBy, PolicyID: settings.PolicyID, SourceRequestID: request.ID, SourceMessageID: message.ID})
		if err != nil {
			return err
		}
		request.CommentID = draft.ID
		if err := p.saveReplyRequest(ctx, taskID, &request); err != nil {
			return err
		}
	} else if draft.SourceMessageID != message.ID || draft.Notes != proposal.Notes || draft.PrivateNotes != *proposal.PrivateNotes {
		return hold("The final draft changed after its publication preview was stored.")
	}
	result, err := p.publishComment(ctx, client, issue, draft, true, "automatic-policy:"+settings.PolicyID)
	if err != nil {
		return hold(err.Error())
	}
	request.PublicationState, request.PublicationError = result.State, ""
	return p.saveReplyRequest(ctx, taskID, &request)
}
