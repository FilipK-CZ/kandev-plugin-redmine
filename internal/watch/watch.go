// Package watch implements structured-filter issue watches: one Kandev task
// per newly matching Redmine issue, deduplicated by (issue_watch_id,
// issue_id), with a per-watch maxInflightTasks throttle that actually
// enforces. Watcher-created tasks carry plugin:<id> provenance (stamped by
// the host on every Tasks().Create, per pluginsdk's Host.Tasks() doc) so
// PluginOwnedTaskTrees cleans them up when the watch or connection is
// removed.
package watch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"kandev-plugin-redmine/internal/fieldmapping"
	"kandev-plugin-redmine/internal/issues"
	"kandev-plugin-redmine/internal/tasklink"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// metadataKeyWatchID is the single source of truth for the task-metadata key
// this package writes at creation (createTask) and reads back at
// throttle-check time (inflightCount). Task 07's highest risk (the exact bug
// Sentry's native integration originally shipped with) is these two sites
// disagreeing on the key name — sharing one constant makes that impossible
// by construction rather than by convention.
const metadataKeyWatchID = "redmine_watch_id"
const metadataKeyIssueID = "redmine_issue_id"
const metadataKeyAutomaticIntake = "automatic_intake"
const pluginMetadataKey = "plugin:kandev-plugin-redmine"
const maxTaskTitleRunes = 60

// terminalTaskStates mirrors pkg/api/v1.IsTerminalTaskState's value set
// (COMPLETED/FAILED/CANCELLED) — a watcher task in any other state still
// counts against maxInflightTasks.
var terminalTaskStates = map[string]bool{"COMPLETED": true, "FAILED": true, "CANCELLED": true}

// Watch is one structured issue-watch definition.
type Watch struct {
	ID             string
	WorkspaceID    string
	WorkflowID     string
	WorkflowStepID string
	ProjectID      int
	RepositoryID   string
	BaseBranch     string
	TrackerID      *int
	StatusID       *int
	PriorityID     *int
	AssigneeID     *int
	CategoryID     *int
	// CustomFieldFilters is keyed by Redmine custom-field ID. Values are the
	// live choices selected in the settings UI and are sent as cf_<id> query
	// filters to Redmine.
	CustomFieldFilters map[int]string
	// Filters carries Redmine-native advanced predicates selected through the
	// dynamic "Add filter" UI. The older typed fields above remain for
	// backwards-compatible saved watches.
	Filters          []Filter
	PriorityMappings map[int]string
	// Resolved from current workspace settings on every poll, not watch state.
	TrackerRoutes        []fieldmapping.TrackerRoute
	EntrySteps           map[string][]pluginsdk.WorkflowStep
	UnavailableWorkflows map[string]error
	MaxInflightTasks     int // 0 = unlimited
	Enabled              bool
}

// Filter is a validated Redmine issue-list predicate.
type Filter struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

func (w Watch) matches(issue issues.Issue) bool {
	if w.TrackerID != nil && *w.TrackerID != issue.TrackerID {
		return false
	}
	if w.StatusID != nil && *w.StatusID != issue.StatusID {
		return false
	}
	if w.PriorityID != nil && *w.PriorityID != issue.PriorityID {
		return false
	}
	if w.AssigneeID != nil && *w.AssigneeID != issue.AssigneeID {
		return false
	}
	if w.CategoryID != nil && *w.CategoryID != issue.CategoryID {
		return false
	}
	for id, want := range w.CustomFieldFilters {
		matched := false
		for _, field := range issue.CustomFields {
			if field.ID == id && fmt.Sprint(field.Value) == want {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

const (
	workspaceScope = "workspace"
	watchesKey     = "watches"
	watchScope     = "watch"
	tasksKey       = "tasks"
)

type Service struct {
	host      pluginsdk.Host
	tasklinks *tasklink.Service
	// One mutex deliberately serializes polls with create/update/delete/clear.
	// This prevents duplicate issue tasks and watch resurrection; watch volume
	// is small enough that correctness currently outweighs parallel polling.
	mu sync.Mutex
}

// New requires the plugin's existing task-link service. This makes every
// successfully indexed watcher task participate in the shared sync index.
func New(host pluginsdk.Host, tasklinks *tasklink.Service) *Service {
	return &Service{host: host, tasklinks: tasklinks}
}

// CreateWatch persists a new watch with a fresh ID.
func (s *Service) CreateWatch(ctx context.Context, w Watch) (Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := newWatchID()
	if err != nil {
		return Watch{}, err
	}
	w.ID = id
	if err := s.saveWatchInList(ctx, w); err != nil {
		return Watch{}, err
	}
	return w, nil
}

// UpdateWatch persists changes to an existing watch (identified by w.ID),
// including enabling/disabling it.
func (s *Service) UpdateWatch(ctx context.Context, w Watch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateWatchLocked(ctx, w)
}

func (s *Service) updateWatchLocked(ctx context.Context, w Watch) error {
	if w.ID == "" {
		return fmt.Errorf("watch: id is required for update")
	}
	watches, err := s.listWatchesLocked(ctx, w.WorkspaceID)
	if err != nil {
		return err
	}
	for _, existing := range watches {
		if existing.ID == w.ID {
			return s.saveWatchInList(ctx, w)
		}
	}
	return fmt.Errorf("watch: %s does not belong to workspace %s", w.ID, w.WorkspaceID)
}

// ListWatches returns every watch for workspaceID.
func (s *Service) ListWatches(ctx context.Context, workspaceID string) ([]Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listWatchesLocked(ctx, workspaceID)
}

func (s *Service) listWatchesLocked(ctx context.Context, workspaceID string) ([]Watch, error) {
	value, found, err := s.host.GetState(ctx, workspaceScope, workspaceID, watchesKey)
	if err != nil {
		return nil, fmt.Errorf("watch: reading watches: %w", err)
	}
	if !found {
		return nil, nil
	}
	raw, _ := value["watches"].([]any)
	out := make([]Watch, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, watchFromMap(workspaceID, row))
	}
	return out, nil
}

// DeleteWatch removes the watch and cascade-deletes every task it created
// via PluginOwnedTaskTrees, so disabling/removing a watch never leaves
// orphaned tasks behind.
func (s *Service) DeleteWatch(ctx context.Context, workspaceID, watchID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteWatchLocked(ctx, workspaceID, watchID)
}

func (s *Service) deleteWatchLocked(ctx context.Context, workspaceID, watchID string) error {
	if watchID == "" {
		return fmt.Errorf("watch: id is required for delete")
	}
	watches, err := s.listWatchesLocked(ctx, workspaceID)
	if err != nil {
		return err
	}
	found := false
	for _, w := range watches {
		if w.ID == watchID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("watch: %s does not belong to workspace %s", watchID, workspaceID)
	}
	entries, err := s.host.ListState(ctx, workspaceScope, workspaceID)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Key, "autostart_issue.") && entry.Value["watch_id"] == watchID {
			return fmt.Errorf("watch: an unconfirmed auto-start import requires native inspection before removing this watch")
		}
	}
	tasks, err := s.watchTasks(ctx, workspaceID, watchID)
	if err != nil {
		return err
	}
	for _, taskID := range tasks {
		if err := s.deleteOwnedTaskAndLink(ctx, workspaceID, watchID, taskID); err != nil {
			return err
		}
	}
	if err := s.host.DeleteState(ctx, watchScope, watchStateID(workspaceID, watchID), tasksKey); err != nil {
		return fmt.Errorf("watch: deleting task index: %w", err)
	}
	return s.removeWatchFromList(ctx, workspaceID, watchID)
}

// ClearWorkspace removes all watches and their plugin-owned task trees.
func (s *Service) ClearWorkspace(ctx context.Context, workspaceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	watches, err := s.listWatchesLocked(ctx, workspaceID)
	if err != nil {
		return err
	}
	for _, w := range watches {
		if err := s.deleteWatchLocked(ctx, workspaceID, w.ID); err != nil {
			return err
		}
	}
	return s.host.DeleteState(ctx, workspaceScope, workspaceID, watchesKey)
}

// Poll fetches issues in watch's project, creates one task per newly
// matching, not-yet-seen issue, subject to the maxInflightTasks throttle. A
// disabled watch is a no-op.
func (s *Service) Poll(ctx context.Context, w Watch, issuesSvc *issues.Service) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !w.Enabled {
		return nil
	}
	exists, err := s.watchExistsLocked(ctx, w.WorkspaceID, w.ID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	var failures []error
	autoStarts := 0
	tokenUserID := 0
	var tokenUserErr error
	for _, route := range w.TrackerRoutes {
		if route.AutomaticIntake != nil {
			tokenUserID, tokenUserErr = issuesSvc.TokenUserID(ctx)
			if tokenUserErr != nil {
				failures = append(failures, tokenUserErr)
			}
			break
		}
	}
	owned, err := s.watchTasks(ctx, w.WorkspaceID, w.ID)
	if err != nil {
		return err
	}
	entries, err := s.host.ListState(ctx, workspaceScope, w.WorkspaceID)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Key, "autostart_issue.") && entry.Value["watch_id"] == w.ID {
			failures = append(failures, fmt.Errorf("watch: %s has an unconfirmed earlier attempt (task %v); inspect it before retrying", entry.Key, entry.Value["task_id"]))
		}
	}
	for _, taskID := range owned {
		if tokenUserErr != nil {
			continue
		}
		if err := s.claimIntake(ctx, w, taskID, tokenUserID, issuesSvc); err != nil {
			failures = append(failures, err)
		}
	}
	for offset := 0; ; {
		result, err := issuesSvc.ListIssues(ctx, issues.ListIssuesParams{ProjectID: strconv.Itoa(w.ProjectID), Filters: w.redmineFilters(), NativeFilters: w.nativeFilters(), Offset: offset, Limit: 100})
		if err != nil {
			return err
		}
		for _, issue := range result.Issues {
			if !w.matches(issue) {
				continue
			}
			seen, err := s.hasSeen(ctx, w.WorkspaceID, w.ID, issue.ID)
			if err != nil {
				return err
			}
			if seen {
				continue
			}
			if w.MaxInflightTasks > 0 {
				inflight, err := s.inflightCount(ctx, w)
				if err != nil {
					return err
				}
				if inflight >= w.MaxInflightTasks {
					continue // leave unseen: retry once a slot frees up
				}
			}
			if err := s.createTask(ctx, w, issue, tokenUserID, issuesSvc, &autoStarts); err != nil {
				failures = append(failures, err)
			}
		}
		offset += len(result.Issues)
		if len(result.Issues) == 0 || offset >= result.TotalCount {
			return errors.Join(failures...)
		}
	}
}

func (s *Service) watchExistsLocked(ctx context.Context, workspaceID, watchID string) (bool, error) {
	watches, err := s.listWatchesLocked(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	for _, candidate := range watches {
		if candidate.ID == watchID {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) createTask(ctx context.Context, w Watch, issue issues.Issue, tokenUserID int, issuesSvc *issues.Service, autoStarts *int) error {
	if _, found, err := s.host.GetState(ctx, workspaceScope, w.WorkspaceID, autoStartKey(issue.ID)); err != nil {
		return err
	} else if found {
		return fmt.Errorf("watch: auto-start import of issue %d has an unconfirmed earlier attempt; inspect the native task before retrying", issue.ID)
	}
	if _, linked, err := s.tasklinks.TaskIDForIssue(ctx, w.WorkspaceID, issue.ID); err != nil || linked {
		return err
	}
	var statuses []fieldmapping.StatusMapping
	var intake *intakeClaim
	routed := false
	for _, route := range w.TrackerRoutes {
		if route.RedmineTrackerID == issue.TrackerID {
			w.WorkflowID, w.WorkflowStepID = route.WorkflowID, route.WorkflowStepID
			statuses = route.Statuses
			routed = true
			if err := w.UnavailableWorkflows[w.WorkflowID]; err != nil {
				return fmt.Errorf("watch: workflow %s unavailable for issue %d: %w", w.WorkflowID, issue.ID, err)
			}
			if route.AutomaticIntake != nil {
				entry, err := route.IntakeEntry(w.EntrySteps[route.WorkflowID])
				if err != nil {
					return err
				}
				if tokenUserID <= 0 || issue.StatusID != route.AutomaticIntake.FromStatusID || issue.AssigneeID != tokenUserID {
					return nil
				}
				current, err := issuesSvc.GetIssue(ctx, issue.ID)
				if err != nil {
					return err
				}
				if current.ProjectID != w.ProjectID || current.TrackerID != route.RedmineTrackerID || current.StatusID != route.AutomaticIntake.FromStatusID || current.AssigneeID != tokenUserID || !w.matches(*current) {
					return nil
				}
				issue = *current
				intake = &intakeClaim{State: "pending", IssueID: issue.ID, ProjectID: issue.ProjectID, TrackerID: issue.TrackerID, TokenUserID: tokenUserID, WorkflowID: route.WorkflowID, StepID: route.WorkflowStepID, ProfileID: entry.AgentProfileID, FromStatusID: route.AutomaticIntake.FromStatusID, WorkingStatusID: route.AutomaticIntake.WorkingStatusID}
			}
			break
		}
	}
	if err := w.UnavailableWorkflows[w.WorkflowID]; err != nil {
		return fmt.Errorf("watch: workflow %s unavailable for issue %d: %w", w.WorkflowID, issue.ID, err)
	}
	if step, mapped := (fieldmapping.Mapping{Statuses: statuses}).WorkflowStepForStatus(issue.StatusID); mapped {
		w.WorkflowStepID = step
	}
	startAgent := false
	entryProfileID := ""
	for _, step := range w.EntrySteps[w.WorkflowID] {
		if step.ID != w.WorkflowStepID && !(w.WorkflowStepID == "" && step.IsStartStep) {
			continue
		}
		if !routed && !step.IsStartStep {
			continue
		}
		for _, action := range step.OnEnterActionTypes {
			startAgent = startAgent || action == "auto_start_agent"
		}
		entryProfileID = step.AgentProfileID
	}
	if startAgent {
		if w.MaxInflightTasks <= 0 {
			return fmt.Errorf("watch: automatic workflow starts require a positive max_inflight_tasks limit")
		}
		if entryProfileID == "" {
			return fmt.Errorf("watch: automatic workflow starts require a fixed entry agent profile")
		}
		if *autoStarts >= w.MaxInflightTasks {
			return nil
		}
		*autoStarts += 1 // Failed launches consume this poll's budget too.
		if err := s.host.SetState(ctx, workspaceScope, w.WorkspaceID, autoStartKey(issue.ID), map[string]any{"state": "creating", "issue_id": issue.ID, "watch_id": w.ID}); err != nil {
			return err
		}
	}
	var launch *pluginsdk.PluginTaskLaunchOptions
	if startAgent {
		launch = &pluginsdk.PluginTaskLaunchOptions{AgentProfileID: &entryProfileID}
	}
	var repositories []pluginsdk.PluginTaskRepository
	if w.RepositoryID != "" {
		repository := pluginsdk.PluginTaskRepository{RepositoryID: w.RepositoryID}
		if w.BaseBranch != "" {
			repository.BaseBranch = &w.BaseBranch
		}
		repositories = []pluginsdk.PluginTaskRepository{repository}
	}
	metadata := map[string]any{metadataKeyWatchID: w.ID, metadataKeyIssueID: issue.ID}
	if intake != nil {
		metadata[metadataKeyAutomaticIntake] = map[string]any{"token_user_id": tokenUserID, "from_status_id": intake.FromStatusID, "working_status_id": intake.WorkingStatusID, "workflow_id": intake.WorkflowID, "entry_step_id": intake.StepID, "profile_id": intake.ProfileID, "repository_id": w.RepositoryID}
	}
	task, err := s.host.Tasks().Create(ctx, pluginsdk.CreateTaskInput{
		WorkspaceID:  w.WorkspaceID,
		WorkflowID:   w.WorkflowID,
		StartAgent:   startAgent,
		Launch:       launch,
		Repositories: repositories,
		WorkflowStepID: func() *string {
			if w.WorkflowStepID == "" {
				return nil
			}
			step := w.WorkflowStepID
			return &step
		}(),
		Title:       watcherTaskTitle(issue.ID, issue.Subject),
		Description: issue.Description,
		Priority:    w.PriorityMappings[issue.PriorityID],
		Metadata:    metadata,
	})
	if err != nil {
		if startAgent && (status.Code(err) == codes.PermissionDenied || status.Code(err) == codes.InvalidArgument || status.Code(err) == codes.Unimplemented) {
			err = errors.Join(err, s.host.DeleteState(ctx, workspaceScope, w.WorkspaceID, autoStartKey(issue.ID)))
		}
		return fmt.Errorf("watch: creating task for issue %d: %w", issue.ID, err)
	}
	retain := func(cause error) error {
		if startAgent {
			return fmt.Errorf("watch: auto-start task %s retained for native inspection: %w", task.ID, cause)
		}
		return s.compensateCreatedTask(ctx, task.ID, cause)
	}
	if startAgent {
		if err := s.host.SetState(ctx, workspaceScope, w.WorkspaceID, autoStartKey(issue.ID), map[string]any{"state": "created", "issue_id": issue.ID, "task_id": task.ID, "watch_id": w.ID}); err != nil {
			return retain(err)
		}
	}
	if err := s.tasklinks.Set(ctx, task.ID, w.WorkspaceID, issue.ID, issue.URL); err != nil {
		return retain(fmt.Errorf("watch: linking task %s: %w", task.ID, err))
	}
	if routed {
		if err := s.tasklinks.ObserveStatus(ctx, task.ID, issue.StatusID, issue.TrackerID, false); err != nil {
			return retain(err)
		}
	}
	if intake != nil {
		if err := s.saveIntakeClaim(ctx, task.ID, *intake); err != nil {
			return retain(err)
		}
	}
	if err := s.recordWatchTask(ctx, w.WorkspaceID, w.ID, issue.ID, task.ID); err != nil {
		return retain(fmt.Errorf("watch: recording task %s: %w", task.ID, err))
	}
	if startAgent {
		return s.host.DeleteState(ctx, workspaceScope, w.WorkspaceID, autoStartKey(issue.ID))
	}
	return nil
}

func autoStartKey(issueID int) string { return "autostart_issue." + strconv.Itoa(issueID) }

func (w Watch) redmineFilters() map[string]string {
	filters := make(map[string]string)
	if w.TrackerID != nil {
		filters["tracker_id"] = strconv.Itoa(*w.TrackerID)
	}
	if w.StatusID != nil {
		filters["status_id"] = strconv.Itoa(*w.StatusID)
	}
	if w.PriorityID != nil {
		filters["priority_id"] = strconv.Itoa(*w.PriorityID)
	}
	if w.AssigneeID != nil {
		filters["assigned_to_id"] = strconv.Itoa(*w.AssigneeID)
	}
	if w.CategoryID != nil {
		filters["category_id"] = strconv.Itoa(*w.CategoryID)
	}
	for id, value := range w.CustomFieldFilters {
		if id > 0 && value != "" {
			filters["cf_"+strconv.Itoa(id)] = value
		}
	}
	return filters
}

func (w Watch) nativeFilters() []issues.NativeFilter {
	filters := make([]issues.NativeFilter, 0, len(w.Filters))
	for _, filter := range w.Filters {
		filters = append(filters, issues.NativeFilter{Field: filter.Field, Operator: filter.Operator, Value: filter.Value})
	}
	return filters
}

func watcherTaskTitle(issueID int, subject string) string {
	title := []rune(fmt.Sprintf("Redmine #%d: %s", issueID, subject))
	if len(title) <= maxTaskTitleRunes {
		return string(title)
	}
	return string(title[:maxTaskTitleRunes-1]) + "…"
}

func (s *Service) compensateCreatedTask(ctx context.Context, taskID string, cause error) error {
	var cleanup error
	if err := s.tasklinks.Unset(ctx, taskID); err != nil {
		cleanup = errors.Join(cleanup, fmt.Errorf("compensating link removal: %w", err))
	}
	if manager, ok := pluginsdk.PluginOwnedTaskTrees(s.host); ok {
		if _, err := manager.Delete(ctx, taskID); err != nil {
			cleanup = errors.Join(cleanup, fmt.Errorf("compensating task delete: %w", err))
		}
	} else {
		cleanup = errors.Join(cleanup, errors.New("compensating task delete: host lacks PluginOwnedTaskTrees"))
	}
	if cleanup != nil {
		return errors.Join(cause, cleanup)
	}
	return cause
}

func (s *Service) deleteOwnedTaskAndLink(ctx context.Context, workspaceID, watchID, taskID string) error {
	manager, ok := pluginsdk.PluginOwnedTaskTrees(s.host)
	if !ok {
		return errors.New("watch: host lacks PluginOwnedTaskTrees for cascade cleanup")
	}
	link, found, err := s.tasklinks.Get(ctx, taskID)
	if err != nil {
		return fmt.Errorf("watch: reading link for task %s: %w", taskID, err)
	}
	if found && link.WorkspaceID != workspaceID {
		return fmt.Errorf("watch: refusing to cascade task %s with link owned by workspace %s", taskID, link.WorkspaceID)
	}
	task, err := s.host.Tasks().Get(ctx, taskID)
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("watch: reading task %s before cascade: %w", taskID, err)
	}
	if err == nil && (task.WorkspaceID != workspaceID || taskPluginMetadata(task)[metadataKeyWatchID] != watchID) {
		return fmt.Errorf("watch: refusing to cascade task %s outside workspace/watch ownership", taskID)
	}
	if _, err := manager.Delete(ctx, taskID); err != nil {
		return fmt.Errorf("watch: cascading delete for task %s: %w", taskID, err)
	}
	if found {
		if err := s.tasklinks.Unset(ctx, taskID); err != nil {
			return fmt.Errorf("watch: removing link for task %s: %w", taskID, err)
		}
	}
	return nil
}

// inflightCount counts this watch's created tasks that are not yet in a
// terminal state — the throttle's live enforcement point. It reads
// metadataKeyWatchID back from each task, but only tasks this package's own
// index already associates with the watch are examined (the index is the
// authoritative per-watch task list; the metadata key is what a mismatch
// bug would silently break — see the constant's doc comment).
func (s *Service) inflightCount(ctx context.Context, w Watch) (int, error) {
	tasks, err := s.watchTasks(ctx, w.WorkspaceID, w.ID)
	if err != nil {
		return 0, err
	}
	count := 0
	entries, err := s.host.ListState(ctx, workspaceScope, w.WorkspaceID)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Key, "autostart_issue.") || entry.Value["watch_id"] != w.ID {
			continue
		}
		indexed := false
		for _, taskID := range tasks {
			if taskID == entry.Value["task_id"] {
				indexed = true
			}
		}
		if !indexed {
			count++
		} // An unknown launch still occupies a slot.
	}
	for _, taskID := range tasks {
		task, err := s.host.Tasks().Get(ctx, taskID)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				continue // deleted task does not count as inflight
			}
			return 0, fmt.Errorf("watch: reading task %s for throttle: %w", taskID, err)
		}
		if taskPluginMetadata(task)[metadataKeyWatchID] != w.ID {
			continue
		}
		if !terminalTaskStates[task.State] {
			count++
		}
	}
	return count, nil
}

func taskPluginMetadata(task *pluginsdk.Task) map[string]any {
	if task == nil {
		return nil
	}
	if nested, ok := task.Metadata[pluginMetadataKey].(map[string]any); ok {
		return nested
	}
	// Older test hosts and SDK candidates returned plugin-owned metadata
	// already unwrapped. Retain the fallback so upgrading the host does not
	// invalidate existing watcher ownership records.
	return task.Metadata
}

func (s *Service) hasSeen(ctx context.Context, workspaceID, watchID string, issueID int) (bool, error) {
	tasks, err := s.watchTasks(ctx, workspaceID, watchID)
	if err != nil {
		return false, err
	}
	_, ok := tasks[issueID]
	return ok, nil
}

func watchStateID(workspaceID, watchID string) string { return workspaceID + ":" + watchID }

func (s *Service) watchTasks(ctx context.Context, workspaceID, watchID string) (map[int]string, error) {
	value, found, err := s.host.GetState(ctx, watchScope, watchStateID(workspaceID, watchID), tasksKey)
	if err != nil {
		return nil, fmt.Errorf("watch: reading task index: %w", err)
	}
	out := make(map[int]string)
	if !found {
		return out, nil
	}
	raw, _ := value["issue_id_to_task_id"].(map[string]any)
	for issueIDStr, taskID := range raw {
		issueID, err := strconv.Atoi(issueIDStr)
		if err != nil {
			continue
		}
		if id, ok := taskID.(string); ok {
			out[issueID] = id
		}
	}
	return out, nil
}

func (s *Service) recordWatchTask(ctx context.Context, workspaceID, watchID string, issueID int, taskID string) error {
	tasks, err := s.watchTasks(ctx, workspaceID, watchID)
	if err != nil {
		return err
	}
	tasks[issueID] = taskID
	values := make(map[string]any, len(tasks))
	for id, tID := range tasks {
		values[strconv.Itoa(id)] = tID
	}
	if err := s.host.SetState(ctx, watchScope, watchStateID(workspaceID, watchID), tasksKey, map[string]any{"issue_id_to_task_id": values}); err != nil {
		return fmt.Errorf("watch: saving task index: %w", err)
	}
	return nil
}

func (s *Service) saveWatchInList(ctx context.Context, w Watch) error {
	watches, err := s.listWatchesLocked(ctx, w.WorkspaceID)
	if err != nil {
		return err
	}
	replaced := false
	for i, existing := range watches {
		if existing.ID == w.ID {
			watches[i] = w
			replaced = true
			break
		}
	}
	if !replaced {
		watches = append(watches, w)
	}
	return s.saveWatchList(ctx, w.WorkspaceID, watches)
}

func (s *Service) removeWatchFromList(ctx context.Context, workspaceID, watchID string) error {
	watches, err := s.listWatchesLocked(ctx, workspaceID)
	if err != nil {
		return err
	}
	kept := make([]Watch, 0, len(watches))
	for _, w := range watches {
		if w.ID != watchID {
			kept = append(kept, w)
		}
	}
	return s.saveWatchList(ctx, workspaceID, kept)
}

func (s *Service) saveWatchList(ctx context.Context, workspaceID string, watches []Watch) error {
	values := make([]any, len(watches))
	for i, w := range watches {
		values[i] = w.toMap()
	}
	if err := s.host.SetState(ctx, workspaceScope, workspaceID, watchesKey, map[string]any{"watches": values}); err != nil {
		return fmt.Errorf("watch: saving watches: %w", err)
	}
	return nil
}

func (w Watch) toMap() map[string]any {
	m := map[string]any{
		"id":                 w.ID,
		"workflow_id":        w.WorkflowID,
		"workflow_step_id":   w.WorkflowStepID,
		"project_id":         w.ProjectID,
		"repository_id":      w.RepositoryID,
		"base_branch":        w.BaseBranch,
		"max_inflight_tasks": w.MaxInflightTasks,
		"enabled":            w.Enabled,
	}
	if w.TrackerID != nil {
		m["tracker_id"] = *w.TrackerID
	}
	if w.StatusID != nil {
		m["status_id"] = *w.StatusID
	}
	if w.PriorityID != nil {
		m["priority_id"] = *w.PriorityID
	}
	if w.AssigneeID != nil {
		m["assignee_id"] = *w.AssigneeID
	}
	if w.CategoryID != nil {
		m["category_id"] = *w.CategoryID
	}
	if len(w.CustomFieldFilters) > 0 {
		m["custom_field_filters"] = intStringMap(w.CustomFieldFilters)
	}
	if len(w.Filters) > 0 {
		filters := make([]any, 0, len(w.Filters))
		for _, filter := range w.Filters {
			filters = append(filters, map[string]any{"field": filter.Field, "operator": filter.Operator, "value": filter.Value})
		}
		m["filters"] = filters
	}
	if len(w.PriorityMappings) > 0 {
		m["priority_mappings"] = intStringMap(w.PriorityMappings)
	}
	return m
}

func watchFromMap(workspaceID string, m map[string]any) Watch {
	w := Watch{WorkspaceID: workspaceID}
	w.RepositoryID, _ = m["repository_id"].(string)
	w.BaseBranch, _ = m["base_branch"].(string)
	if v, ok := m["id"].(string); ok {
		w.ID = v
	}
	if v, ok := m["workflow_id"].(string); ok {
		w.WorkflowID = v
	}
	if v, ok := m["workflow_step_id"].(string); ok {
		w.WorkflowStepID = v
	}
	if v, ok := m["project_id"].(float64); ok {
		w.ProjectID = int(v)
	}
	if v, ok := m["max_inflight_tasks"].(float64); ok {
		w.MaxInflightTasks = int(v)
	}
	if v, ok := m["enabled"].(bool); ok {
		w.Enabled = v
	}
	if raw, ok := m["filters"].([]any); ok {
		for _, item := range raw {
			filterMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			field, _ := filterMap["field"].(string)
			operator, _ := filterMap["operator"].(string)
			value, _ := filterMap["value"].(string)
			if field != "" && operator != "" && value != "" {
				w.Filters = append(w.Filters, Filter{Field: field, Operator: operator, Value: value})
			}
		}
	}
	if v, ok := m["tracker_id"].(float64); ok {
		id := int(v)
		w.TrackerID = &id
	}
	if v, ok := m["status_id"].(float64); ok {
		id := int(v)
		w.StatusID = &id
	}
	if v, ok := m["priority_id"].(float64); ok {
		id := int(v)
		w.PriorityID = &id
	}
	if v, ok := m["assignee_id"].(float64); ok {
		id := int(v)
		w.AssigneeID = &id
	}
	if v, ok := m["category_id"].(float64); ok {
		id := int(v)
		w.CategoryID = &id
	}
	w.CustomFieldFilters = stringMapToIntMap(m["custom_field_filters"])
	w.PriorityMappings = stringMapToIntMap(m["priority_mappings"])
	return w
}

func intStringMap(input map[int]string) map[string]any {
	out := make(map[string]any, len(input))
	for id, value := range input {
		out[strconv.Itoa(id)] = value
	}
	return out
}

func stringMapToIntMap(value any) map[int]string {
	raw, _ := value.(map[string]any)
	out := make(map[int]string, len(raw))
	for id, mapped := range raw {
		parsed, err := strconv.Atoi(id)
		if err == nil {
			if text, ok := mapped.(string); ok {
				out[parsed] = text
			}
		}
	}
	return out
}

func newWatchID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("watch: generating id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
