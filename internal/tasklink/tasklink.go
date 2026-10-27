// Package tasklink persists the link between a Kandev task and a Redmine
// issue, plus the echo-suppression bookkeeping internal/sync needs to keep a
// write-back round trip from bouncing the task. The host's Tasks().Update has
// no metadata field to write a link onto an existing task (see
// docs/plans/redmine-plugin/task-06-task-linking-bidirectional-sync.md
// "Plan deviations"), so the link itself is plugin-owned state — the same
// pattern kandev-plugin-bitbucket uses for its own task-scoped link/unlink
// actions.
package tasklink

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// Link is the persisted state for one linked task.
type Link struct {
	IssueID     int
	IssueURL    string
	WorkspaceID string

	// Echo suppression: the most recent status this plugin itself pushed
	// outbound, compared against inbound observations before applying them
	// (spec "Bidirectional sync"). Title/description are inbound-only — no
	// outbound echo is recorded for them.
	LastPushedStatusID *int
	ObservedStatusID   int
	ObservedTrackerID  int
}

const (
	taskScope      = "task"
	linkKey        = "link"
	workspaceScope = "workspace"
	indexKey       = "issue_links"
)

type Service struct {
	host pluginsdk.Host
	mu   sync.Mutex
}

func New(host pluginsdk.Host) *Service {
	return &Service{host: host}
}

// Set links taskID to a Redmine issue and adds it to the workspace's reverse
// index (issue ID -> task ID) the sync/watch loops use.
func (s *Service) Set(ctx context.Context, taskID, workspaceID string, issueID int, issueURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, err := s.index(ctx, workspaceID)
	if err != nil {
		return err
	}
	if existing, ok := index[strconv.Itoa(issueID)]; ok && existing != taskID {
		return fmt.Errorf("tasklink: Redmine issue %d is already linked to task %s", issueID, existing)
	}
	previous, found, err := s.getLocked(ctx, taskID)
	if err != nil {
		return err
	}
	link := Link{IssueID: issueID, IssueURL: issueURL, WorkspaceID: workspaceID}
	oldSnapshot, err := s.indexSnapshot(ctx, workspaceID)
	if err != nil {
		return err
	}
	var priorSnapshot indexState
	if found && previous.WorkspaceID != workspaceID {
		priorSnapshot, err = s.indexSnapshot(ctx, previous.WorkspaceID)
		if err != nil {
			return err
		}
	} else {
		priorSnapshot = oldSnapshot
	}
	if err := s.save(ctx, taskID, link); err != nil {
		return err
	}
	if found && (previous.WorkspaceID != workspaceID || previous.IssueID != issueID) {
		if err := s.removeFromIndexIfOwned(ctx, previous.WorkspaceID, previous.IssueID, taskID); err != nil {
			return s.rollbackSet(ctx, taskID, previous, found, oldSnapshot, previous.WorkspaceID, priorSnapshot, err)
		}
	}
	if err := s.addToIndex(ctx, workspaceID, issueID, taskID); err != nil {
		return s.rollbackSet(ctx, taskID, previous, found, oldSnapshot, previous.WorkspaceID, priorSnapshot, err)
	}
	return nil
}

// Get returns the current link for taskID, if any.
func (s *Service) Get(ctx context.Context, taskID string) (*Link, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(ctx, taskID)
}

func (s *Service) getLocked(ctx context.Context, taskID string) (*Link, bool, error) {
	value, found, err := s.host.GetState(ctx, taskScope, taskID, linkKey)
	if err != nil {
		return nil, false, fmt.Errorf("tasklink: reading link: %w", err)
	}
	if !found {
		return nil, false, nil
	}
	link := linkFromMap(value)
	return &link, true, nil
}

// Unset removes the link and its reverse-index entry. A no-op if taskID
// isn't linked.
func (s *Service) Unset(ctx context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, found, err := s.getLocked(ctx, taskID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	index, err := s.indexSnapshot(ctx, link.WorkspaceID)
	if err != nil {
		return err
	}
	if err := s.host.DeleteState(ctx, taskScope, taskID, linkKey); err != nil {
		return fmt.Errorf("tasklink: deleting link: %w", err)
	}
	if err := s.removeFromIndexIfOwned(ctx, link.WorkspaceID, link.IssueID, taskID); err != nil {
		rollbackErr := s.restoreLinkAndIndex(ctx, taskID, link, index)
		return wrapRollbackError(err, rollbackErr)
	}
	return nil
}

type indexState struct {
	workspaceID string
	found       bool
	value       map[string]string
}

func (s *Service) indexSnapshot(ctx context.Context, workspaceID string) (indexState, error) {
	value, found, err := s.host.GetState(ctx, workspaceScope, workspaceID, indexKey)
	if err != nil {
		return indexState{}, fmt.Errorf("tasklink: reading issue index: %w", err)
	}
	index := make(map[string]string)
	if found {
		if raw, ok := value["issue_id_to_task_id"].(map[string]any); ok {
			for issue, task := range raw {
				if id, ok := task.(string); ok {
					index[issue] = id
				}
			}
		}
	}
	return indexState{workspaceID: workspaceID, found: found, value: index}, nil
}

func (s *Service) restoreIndex(ctx context.Context, snapshot indexState) error {
	if !snapshot.found {
		return s.host.DeleteState(ctx, workspaceScope, snapshot.workspaceID, indexKey)
	}
	return s.saveIndex(ctx, snapshot.workspaceID, snapshot.value)
}

func (s *Service) restoreLinkAndIndex(ctx context.Context, taskID string, link *Link, index indexState) error {
	if err := s.save(ctx, taskID, *link); err != nil {
		return err
	}
	return s.restoreIndex(ctx, index)
}

func (s *Service) rollbackSet(ctx context.Context, taskID string, previous *Link, hadPrevious bool, newIndex indexState, oldWorkspace string, oldIndex indexState, cause error) error {
	var rollbackErr error
	if hadPrevious {
		rollbackErr = s.save(ctx, taskID, *previous)
	} else {
		rollbackErr = s.host.DeleteState(ctx, taskScope, taskID, linkKey)
	}
	if err := s.restoreIndex(ctx, newIndex); err != nil && rollbackErr == nil {
		rollbackErr = err
	}
	if oldWorkspace != newIndex.workspaceID {
		if err := s.restoreIndex(ctx, oldIndex); err != nil && rollbackErr == nil {
			rollbackErr = err
		}
	}
	return wrapRollbackError(cause, rollbackErr)
}

func wrapRollbackError(cause, rollbackErr error) error {
	if rollbackErr == nil {
		return cause
	}
	return fmt.Errorf("tasklink: operation failed and rollback failed: %w", errors.Join(cause, rollbackErr))
}

// TaskIDForIssue resolves the reverse index: which task, if any, is linked
// to issueID within workspaceID. Scoped by workspace so two workspaces'
// links never resolve into each other, even if both happen to reference the
// same numeric issue ID on two different Redmine instances.
func (s *Service) TaskIDForIssue(ctx context.Context, workspaceID string, issueID int) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, err := s.index(ctx, workspaceID)
	if err != nil {
		return "", false, err
	}
	taskID, ok := index[strconv.Itoa(issueID)]
	return taskID, ok, nil
}

// ClearWorkspace deletes every task-scoped link named by this workspace's
// reverse index before removing the index itself.
func (s *Service) ClearWorkspace(ctx context.Context, workspaceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, err := s.index(ctx, workspaceID)
	if err != nil {
		return err
	}
	for _, taskID := range index {
		if err := s.host.DeleteState(ctx, taskScope, taskID, linkKey); err != nil {
			return fmt.Errorf("tasklink: clearing task link: %w", err)
		}
	}
	if err := s.host.DeleteState(ctx, workspaceScope, workspaceID, indexKey); err != nil {
		return fmt.Errorf("tasklink: clearing issue index: %w", err)
	}
	return nil
}

// RecordPushedStatus records the status this plugin itself just pushed
// outbound, for echo suppression on the next inbound poll.
func (s *Service) RecordPushedStatus(ctx context.Context, taskID string, statusID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, found, err := s.getLocked(ctx, taskID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("tasklink: task %s is not linked", taskID)
	}
	link.LastPushedStatusID = &statusID
	return s.save(ctx, taskID, *link)
}

// ConsumeStatusEcho clears the outbound status marker after the next inbound
// observation. The marker is intentionally one-shot: a later independent
// Redmine update back to the same value must not be suppressed forever.
func (s *Service) ConsumeStatusEcho(ctx context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, found, err := s.getLocked(ctx, taskID)
	if err != nil || !found {
		return err
	}
	link.LastPushedStatusID = nil
	return s.save(ctx, taskID, *link)
}

// ObserveStatus remembers external transitions without replaying an unchanged
// routed status over the task's internal workflow progress.
func (s *Service) ObserveStatus(ctx context.Context, taskID string, statusID, trackerID int, consumeEcho bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, found, err := s.getLocked(ctx, taskID)
	if err != nil || !found {
		return err
	}
	if link.ObservedStatusID == statusID && link.ObservedTrackerID == trackerID && !consumeEcho {
		return nil
	}
	link.ObservedStatusID, link.ObservedTrackerID = statusID, trackerID
	if consumeEcho {
		link.LastPushedStatusID = nil
	}
	return s.save(ctx, taskID, *link)
}

func (s *Service) save(ctx context.Context, taskID string, link Link) error {
	if err := s.host.SetState(ctx, taskScope, taskID, linkKey, link.toMap()); err != nil {
		return fmt.Errorf("tasklink: saving link: %w", err)
	}
	return nil
}

func (s *Service) index(ctx context.Context, workspaceID string) (map[string]string, error) {
	value, found, err := s.host.GetState(ctx, workspaceScope, workspaceID, indexKey)
	if err != nil {
		return nil, fmt.Errorf("tasklink: reading issue index: %w", err)
	}
	index := make(map[string]string)
	if !found {
		return index, nil
	}
	raw, _ := value["issue_id_to_task_id"].(map[string]any)
	for issueID, taskID := range raw {
		if id, ok := taskID.(string); ok {
			index[issueID] = id
		}
	}
	return index, nil
}

func (s *Service) addToIndex(ctx context.Context, workspaceID string, issueID int, taskID string) error {
	index, err := s.index(ctx, workspaceID)
	if err != nil {
		return err
	}
	index[strconv.Itoa(issueID)] = taskID
	return s.saveIndex(ctx, workspaceID, index)
}

func (s *Service) removeFromIndex(ctx context.Context, workspaceID string, issueID int) error {
	index, err := s.index(ctx, workspaceID)
	if err != nil {
		return err
	}
	delete(index, strconv.Itoa(issueID))
	return s.saveIndex(ctx, workspaceID, index)
}

func (s *Service) removeFromIndexIfOwned(ctx context.Context, workspaceID string, issueID int, taskID string) error {
	index, err := s.index(ctx, workspaceID)
	if err != nil {
		return err
	}
	key := strconv.Itoa(issueID)
	if index[key] != taskID {
		return nil
	}
	delete(index, key)
	return s.saveIndex(ctx, workspaceID, index)
}

func (s *Service) saveIndex(ctx context.Context, workspaceID string, index map[string]string) error {
	values := make(map[string]any, len(index))
	for issueID, taskID := range index {
		values[issueID] = taskID
	}
	if err := s.host.SetState(ctx, workspaceScope, workspaceID, indexKey, map[string]any{"issue_id_to_task_id": values}); err != nil {
		return fmt.Errorf("tasklink: saving issue index: %w", err)
	}
	return nil
}

func (l Link) toMap() map[string]any {
	m := map[string]any{
		"issue_id":            l.IssueID,
		"issue_url":           l.IssueURL,
		"workspace_id":        l.WorkspaceID,
		"observed_status_id":  l.ObservedStatusID,
		"observed_tracker_id": l.ObservedTrackerID,
	}
	if l.LastPushedStatusID != nil {
		m["last_pushed_status_id"] = *l.LastPushedStatusID
	}
	return m
}

func linkFromMap(m map[string]any) Link {
	link := Link{}
	if v, ok := m["issue_id"].(float64); ok {
		link.IssueID = int(v)
	}
	if v, ok := m["issue_url"].(string); ok {
		link.IssueURL = v
	}
	if v, ok := m["workspace_id"].(string); ok {
		link.WorkspaceID = v
	}
	if v, ok := m["last_pushed_status_id"].(float64); ok {
		statusID := int(v)
		link.LastPushedStatusID = &statusID
	}
	if v, ok := m["observed_status_id"].(float64); ok {
		link.ObservedStatusID = int(v)
	}
	if v, ok := m["observed_tracker_id"].(float64); ok {
		link.ObservedTrackerID = int(v)
	}
	// Legacy last_pushed_title / last_pushed_description_hash entries are
	// Legacy applied_tracker_label / last_pushed_title /
	// last_pushed_description_hash entries are intentionally ignored and
	// dropped on the next save.
	return link
}
