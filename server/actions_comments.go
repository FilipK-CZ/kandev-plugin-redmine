package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"kandev-plugin-redmine/internal/connection"
	"kandev-plugin-redmine/internal/issues"
	"kandev-plugin-redmine/internal/redmineclient"
)

const commentCurrentKey = "comment.current"
const maxCommentBytes = 32 * 1024

type commentProposal struct {
	IssueID      int    `json:"issue_id"`
	Notes        string `json:"notes"`
	PrivateNotes *bool  `json:"private_notes"`
	RequestID    string `json:"request_id,omitempty"`
}

type commentDraft struct {
	ID                 string `json:"id"`
	WorkspaceID        string `json:"workspace_id"`
	TaskID             string `json:"task_id"`
	IssueID            int    `json:"issue_id"`
	IssueURL           string `json:"issue_url"`
	Subject            string `json:"subject"`
	Notes              string `json:"notes"`
	PrivateNotes       bool   `json:"private_notes"`
	ActorID            string `json:"actor_id"`
	WriterID           int    `json:"writer_id"`
	ConnectionRevision string `json:"connection_revision"`
	ContextHash        string `json:"context_hash"`
	LastJournalID      int    `json:"last_journal_id"`
	State              string `json:"state"`
	PreparedAt         string `json:"prepared_at"`
	ApprovedAt         string `json:"approved_at,omitempty"`
	CancelledAt        string `json:"cancelled_at,omitempty"`
	ApprovedBy         string `json:"approved_by,omitempty"`
	JournalID          int    `json:"journal_id,omitempty"`
	PolicyID           string `json:"policy_id,omitempty"`
	SourceRequestID    string `json:"source_request_id,omitempty"`
	SourceMessageID    string `json:"source_message_id,omitempty"`
}

func (p *redminePlugin) commentContext(ctx context.Context, req *pluginsdk.PluginActionRequest) (*redmineclient.Client, *connection.Record, *issues.Issue, error) {
	if req.Context.ActorID == "" || req.Context.TaskID == "" || req.Context.WorkspaceID == "" {
		return nil, nil, nil, fmt.Errorf("redmine: comments require an authenticated, task-scoped action")
	}
	return p.linkedCommentContext(ctx, req.Context.WorkspaceID, req.Context.TaskID)
}

func (p *redminePlugin) linkedCommentContext(ctx context.Context, workspaceID, taskID string) (*redmineclient.Client, *connection.Record, *issues.Issue, error) {
	task, err := p.Host().Tasks().Get(ctx, taskID)
	if err != nil {
		return nil, nil, nil, err
	}
	if task == nil || task.WorkspaceID != workspaceID || task.ArchivedAt != nil {
		return nil, nil, nil, fmt.Errorf("redmine: comment task is unavailable in this workspace")
	}
	link, found, err := p.tasklinkSvc.Get(ctx, taskID)
	if err != nil {
		return nil, nil, nil, err
	}
	if !found || link.WorkspaceID != workspaceID {
		return nil, nil, nil, fmt.Errorf("redmine: link this task to a Redmine issue first")
	}
	enabled, err := p.connectionSvc.GetEnabled(ctx, workspaceID)
	if err != nil {
		return nil, nil, nil, err
	}
	if !enabled {
		return nil, nil, nil, fmt.Errorf("redmine: integration is paused")
	}
	conn, found, err := p.connectionSvc.Get(ctx, workspaceID)
	if err != nil {
		return nil, nil, nil, err
	}
	if !found {
		return nil, nil, nil, fmt.Errorf("redmine: workspace has no connection")
	}
	client, err := p.connectionSvc.Client(ctx, workspaceID)
	if err != nil {
		return nil, nil, nil, err
	}
	issue, err := issues.New(client).GetIssue(ctx, link.IssueID)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := p.requireSelectedProject(ctx, workspaceID, issue.ProjectID); err != nil {
		return nil, nil, nil, err
	}
	return client, conn, issue, nil
}

func commentContextHash(issue *issues.Issue) string {
	encoded, _ := json.Marshal(issue)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func (p *redminePlugin) currentComment(ctx context.Context, taskID string) (*commentDraft, error) {
	value, found, err := p.Host().GetState(ctx, "task", taskID, commentCurrentKey)
	if err != nil || !found {
		return nil, err
	}
	id, ok := value["id"].(string)
	if !ok {
		return nil, fmt.Errorf("redmine: comment state is invalid")
	}
	return p.loadComment(ctx, taskID, id)
}

func (p *redminePlugin) loadComment(ctx context.Context, taskID, id string) (*commentDraft, error) {
	if decoded, err := hex.DecodeString(id); err != nil || len(decoded) != 16 {
		return nil, fmt.Errorf("redmine: invalid draft id")
	}
	value, found, err := p.Host().GetState(ctx, "task", taskID, "comment."+id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("redmine: draft not found")
	}
	raw, ok := value["record"].(string)
	var draft commentDraft
	if !ok || json.Unmarshal([]byte(raw), &draft) != nil || draft.ID != id || draft.TaskID != taskID {
		return nil, fmt.Errorf("redmine: comment state is invalid")
	}
	return &draft, nil
}

func (p *redminePlugin) saveComment(ctx context.Context, draft *commentDraft) error {
	encoded, err := json.Marshal(draft)
	if err != nil {
		return err
	}
	return p.Host().SetState(ctx, "task", draft.TaskID, "comment."+draft.ID, map[string]any{"record": string(encoded)})
}

func commentPending(draft *commentDraft) bool {
	return draft != nil && (draft.State == "sending" || draft.State == "unverified")
}

func (p *redminePlugin) handleCommentPrepare(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	var body struct {
		commentProposal
		ExpectedID          string `json:"expected_id"`
		ExpectedContextHash string `json:"expected_context_hash"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return nil, err
	}
	if !normalizeCommentProposal(&body.commentProposal) {
		return classifiedErrorResponse(fmt.Errorf("redmine: provide a comment up to 32 KiB and explicitly select public or private visibility"))
	}
	p.commentMu.Lock()
	defer p.commentMu.Unlock()
	client, conn, issue, err := p.commentContext(ctx, req)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	current, err := p.currentComment(ctx, req.Context.TaskID)
	if err != nil {
		return nil, err
	}
	id := ""
	if current != nil {
		id = current.ID
	}
	replaceable := current == nil || current.State == "prepared" || current.State == "published" || current.State == "rejected" || current.State == "cancelled"
	if id != body.ExpectedID || !replaceable {
		return classifiedErrorResponse(fmt.Errorf("redmine: draft changed or an earlier publication is uncertain; reload and check its result"))
	}
	if issue.ID != body.IssueID || issue.UpdatedOn == "" {
		return classifiedErrorResponse(fmt.Errorf("redmine: the linked issue changed or its version is unavailable"))
	}
	if body.ExpectedContextHash == "" || body.ExpectedContextHash != commentContextHash(issue) {
		return classifiedErrorResponse(fmt.Errorf("redmine: issue changed since loading; reload and review its current context"))
	}
	draft, err := p.prepareComment(ctx, client, conn, issue, &commentDraft{WorkspaceID: req.Context.WorkspaceID, TaskID: req.Context.TaskID, Notes: body.Notes, PrivateNotes: *body.PrivateNotes, ActorID: req.Context.ActorID})
	if err != nil {
		return classifiedErrorResponse(err)
	}
	return jsonResponse(draft)
}

func (p *redminePlugin) prepareComment(ctx context.Context, client *redmineclient.Client, conn *connection.Record, issue *issues.Issue, draft *commentDraft) (*commentDraft, error) {
	user, err := client.ValidateCredentials(ctx)
	if err != nil {
		return nil, err
	}
	if user.ID <= 0 {
		return nil, fmt.Errorf("redmine: publishing identity is unavailable")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	draft.ID, draft.IssueID, draft.IssueURL, draft.Subject = hex.EncodeToString(nonce[:]), issue.ID, issue.URL, issue.Subject
	draft.WriterID, draft.ConnectionRevision, draft.ContextHash = user.ID, conn.Revision, commentContextHash(issue)
	draft.State, draft.PreparedAt = "prepared", time.Now().UTC().Format(time.RFC3339Nano)
	for _, journal := range issue.Journals {
		if journal.ID > draft.LastJournalID {
			draft.LastJournalID = journal.ID
		}
	}
	if err := p.saveComment(ctx, draft); err != nil {
		return nil, err
	}
	if err := p.Host().SetState(ctx, "task", draft.TaskID, commentCurrentKey, map[string]any{"id": draft.ID}); err != nil {
		return nil, err
	}
	return draft, nil
}

func (p *redminePlugin) handleCommentPublish(ctx context.Context, req *pluginsdk.PluginActionRequest, approve bool) (*pluginsdk.PluginActionResponse, error) {
	body, err := decodeBody[struct {
		DraftID string `json:"draft_id"`
	}](req)
	if err != nil {
		return nil, err
	}
	p.commentMu.Lock()
	defer p.commentMu.Unlock()
	client, conn, issue, err := p.commentContext(ctx, req)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	draft, err := p.loadComment(ctx, req.Context.TaskID, body.DraftID)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	if draft.WorkspaceID != req.Context.WorkspaceID || draft.IssueID != issue.ID || draft.IssueURL != issue.URL || draft.ConnectionRevision != conn.Revision {
		return classifiedErrorResponse(fmt.Errorf("redmine: issue link or connection changed; this approval cannot be used"))
	}
	if approve && draft.State == "prepared" && (draft.ActorID != req.Context.ActorID || draft.PolicyID != "") {
		return classifiedErrorResponse(fmt.Errorf("redmine: prepare a new human-review preview before approving this draft"))
	}
	result, err := p.publishComment(ctx, client, issue, draft, approve, req.Context.ActorID)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	return jsonResponse(result)
}

// Both authorization modes use the same persisted attempt and single-append path.
// Callers hold commentMu and verify their human approval or workspace policy.
func (p *redminePlugin) publishComment(ctx context.Context, client *redmineclient.Client, issue *issues.Issue, draft *commentDraft, approve bool, approvedBy string) (*commentDraft, error) {
	if draft.State == "published" || draft.State == "rejected" || draft.State == "cancelled" {
		return draft, nil
	}
	if draft.State == "prepared" {
		if !approve {
			return draft, nil
		}
		current, err := p.currentComment(ctx, draft.TaskID)
		if err != nil {
			return nil, err
		}
		if current == nil || current.ID != draft.ID || draft.ContextHash != commentContextHash(issue) {
			return nil, fmt.Errorf("redmine: draft or issue changed; prepare a new preview before approving")
		}
		user, err := client.ValidateCredentials(ctx)
		if err != nil {
			return nil, err
		}
		if user.ID != draft.WriterID || user.ID <= 0 {
			return nil, fmt.Errorf("redmine: publishing identity changed")
		}
		latest, err := issues.New(client).GetIssue(ctx, draft.IssueID)
		if err != nil {
			return nil, err
		}
		if draft.ContextHash != commentContextHash(latest) {
			return nil, fmt.Errorf("redmine: issue changed immediately before publication; review its current context")
		}
		draft.State, draft.ApprovedBy, draft.ApprovedAt = "sending", approvedBy, time.Now().UTC().Format(time.RFC3339Nano)
		// Persist the attempt before touching Redmine. An uncertain state write
		// or acknowledgement must never cause another append.
		if err := p.saveComment(ctx, draft); err != nil {
			return nil, err
		}
		if err := issues.New(client).AddComment(ctx, issue.ID, draft.Notes, draft.PrivateNotes); err != nil {
			if apiErr, ok := asAPIError(err); ok && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != 408 && apiErr.StatusCode != 429 {
				draft.State = "rejected"
				if err := p.saveComment(ctx, draft); err != nil {
					return nil, err
				}
				return draft, nil
			}
		}
	} else if !commentPending(draft) {
		return nil, fmt.Errorf("redmine: invalid comment state")
	}
	latest, err := issues.New(client).GetIssue(ctx, draft.IssueID)
	draft.State = "unverified"
	if err == nil {
		var matches []int
		for _, journal := range latest.Journals {
			if journal.ID > draft.LastJournalID && journal.User.ID == draft.WriterID && journal.PrivateNotes == draft.PrivateNotes && strings.TrimSpace(strings.ReplaceAll(journal.Notes, "\r\n", "\n")) == draft.Notes {
				matches = append(matches, journal.ID)
			}
		}
		if len(matches) == 1 {
			draft.State, draft.JournalID = "published", matches[0]
		}
	}
	if err := p.saveComment(ctx, draft); err != nil {
		return nil, err
	}
	return draft, nil
}

func (p *redminePlugin) handleCommentDiscard(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	body, err := decodeBody[struct {
		DraftID string `json:"draft_id"`
	}](req)
	if err != nil {
		return nil, err
	}
	p.commentMu.Lock()
	defer p.commentMu.Unlock()
	if _, _, _, err := p.commentContext(ctx, req); err != nil {
		return classifiedErrorResponse(err)
	}
	draft, err := p.loadComment(ctx, req.Context.TaskID, body.DraftID)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	if draft.WorkspaceID != req.Context.WorkspaceID || draft.ActorID != req.Context.ActorID || draft.State != "prepared" {
		return classifiedErrorResponse(fmt.Errorf("redmine: only the reviewing user can edit an unsubmitted preview"))
	}
	draft.State, draft.CancelledAt = "cancelled", time.Now().UTC().Format(time.RFC3339Nano)
	if err := p.saveComment(ctx, draft); err != nil {
		return nil, err
	}
	return jsonResponse(draft)
}

func (p *redminePlugin) handleCommentGet(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	p.commentMu.Lock()
	defer p.commentMu.Unlock()
	_, _, issue, err := p.commentContext(ctx, req)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	draft, err := p.currentComment(ctx, req.Context.TaskID)
	if err != nil {
		return nil, err
	}
	proposal, warning := p.latestCommentProposal(ctx, req.Context.TaskID, issue.ID)
	request, requestFound, err := p.Host().GetState(ctx, "task", req.Context.TaskID, replyLatestKey)
	if err != nil {
		return nil, err
	}
	if !requestFound {
		request = nil
	}
	canRequest, requestErr := p.canRequestReply(ctx, req.Context.WorkspaceID, req.Context.TaskID, request)
	requestWarning := ""
	if requestErr != nil {
		requestWarning = requestErr.Error()
	} else if !canRequest {
		requestWarning = "The previous draft is still active or waiting for human input."
	}
	currentID := ""
	if draft != nil {
		currentID = draft.ID
		if draft.IssueID != issue.ID || draft.IssueURL != issue.URL {
			if commentPending(draft) {
				return classifiedErrorResponse(fmt.Errorf("redmine: an earlier comment for a different issue has an uncertain outcome"))
			}
			draft = nil
		}
	}
	return jsonResponse(map[string]any{
		"issue": map[string]any{"id": issue.ID, "url": issue.URL, "subject": issue.Subject, "updated_on": issue.UpdatedOn},
		"draft": draft, "proposal": proposal, "draft_warning": warning,
		"can_approve": draft != nil && draft.ActorID == req.Context.ActorID && draft.State == "prepared" && draft.PolicyID == "",
		"current_id":  currentID, "context_hash": commentContextHash(issue), "draft_request": request,
		"can_request_draft": canRequest, "draft_request_warning": requestWarning,
	})
}

func (p *redminePlugin) latestCommentProposal(ctx context.Context, taskID string, issueID int) (*commentProposal, string) {
	last, err := p.latestTaskMessage(ctx, taskID, "")
	if err != nil {
		return nil, err.Error()
	}
	if last == nil || last.AuthorType != "agent" {
		return nil, "No completed draft in the latest task message. Paste or request a reply first."
	}
	proposal := parseCommentProposal(last.Content, issueID)
	if proposal == nil {
		return nil, "The latest task message is not a valid reply for this issue. Review the task before preparing a comment manually."
	}
	return proposal, ""
}

func (p *redminePlugin) latestTaskMessage(ctx context.Context, taskID, sessionID string) (*pluginsdk.Message, error) {
	var last *pluginsdk.Message
	page := pluginsdk.Page{Limit: 100}
	filter := pluginsdk.MessageFilter{TaskIDs: []string{taskID}, Types: []string{"message"}}
	if sessionID != "" {
		filter.SessionIDs = []string{sessionID}
	}
	for n := 0; n < 10; n++ {
		messages, info, err := p.Host().Messages().List(ctx, filter, page)
		if err != nil {
			return nil, fmt.Errorf("draft discovery is unavailable; copy the proposed reply manually")
		}
		for i := range messages {
			if messages[i].TaskID == taskID && messages[i].Type == "message" && (sessionID == "" || messages[i].SessionID == sessionID) {
				value := messages[i]
				last = &value
			}
		}
		if info == nil || !info.HasMore {
			return last, nil
		}
		if info.NextCursor == "" || info.NextCursor == page.Cursor {
			return nil, fmt.Errorf("draft history could not be verified; copy the proposed reply manually")
		}
		page.Cursor = info.NextCursor
	}
	return nil, fmt.Errorf("task history exceeds the draft discovery limit; copy the proposed reply manually")
}

func parseCommentProposal(text string, issueID int) *commentProposal {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "```redmine-comment\n") || !strings.HasSuffix(text, "\n```") {
		return nil
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(text, "```redmine-comment\n"), "\n```")
	var proposal commentProposal
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&proposal) != nil || proposal.IssueID != issueID || !normalizeCommentProposal(&proposal) {
		return nil
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil
	}
	return &proposal
}

func normalizeCommentProposal(proposal *commentProposal) bool {
	proposal.Notes = strings.TrimSpace(strings.ReplaceAll(proposal.Notes, "\r\n", "\n"))
	return proposal.Notes != "" && utf8.ValidString(proposal.Notes) && len(proposal.Notes) <= maxCommentBytes && !strings.ContainsRune(proposal.Notes, 0) && proposal.PrivateNotes != nil
}
