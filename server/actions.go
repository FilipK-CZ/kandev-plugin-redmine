package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"kandev-plugin-redmine/internal/connection"
	"kandev-plugin-redmine/internal/fieldmapping"
	"kandev-plugin-redmine/internal/issues"
	"kandev-plugin-redmine/internal/redmineclient"
	redminesync "kandev-plugin-redmine/internal/sync"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// asAPIError unwraps err into a *redmineclient.APIError, if it is (or
// wraps) one.
func asAPIError(err error) (*redmineclient.APIError, bool) {
	var apiErr *redmineclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// HandleAction answers every actions: entry in manifest.yaml — the plugin's
// own native settings UI calling into this backend via
// host.api.invokeAction. req.Context is host-verified (workspace/task
// membership already checked); every handler derives its plugin_state/secret
// keys from req.Context, never from req.Body, so a forged body can't target
// another workspace's connection or another task's link.
func (p *redminePlugin) HandleAction(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("redmine: missing action request")
	}
	// Keep link/connection changes from racing a reviewed publication.
	switch req.ActionKey {
	case "connection.save", "connection.disconnect", "projects.save", "integration.enabled.save", "link.set", "link.unset", "replies.settings.save":
		p.commentMu.Lock()
		defer p.commentMu.Unlock()
	}
	switch req.ActionKey {
	case "connection.get":
		return p.handleConnectionGet(ctx, req)
	case "connection.save":
		return p.handleConnectionSave(ctx, req)
	case "connection.disconnect":
		return p.handleConnectionDisconnect(ctx, req)
	case "integration.enabled.get":
		return p.handleIntegrationEnabledGet(ctx, req)
	case "integration.enabled.save":
		return p.handleIntegrationEnabledSave(ctx, req)
	case "projects.list":
		return p.handleProjectsList(ctx, req)
	case "projects.save":
		return p.handleProjectsSave(ctx, req)
	case "workflows.list":
		return p.handleWorkflowsList(ctx, req)
	case "fieldmapping.get":
		return p.handleFieldMappingGet(ctx, req)
	case "fieldmapping.save":
		return p.handleFieldMappingSave(ctx, req)
	case "syncoptions.save":
		return p.handleSyncOptionsSave(ctx, req)
	case "syncoptions.get":
		return p.handleSyncOptionsGet(ctx, req)
	case "issues.create":
		return p.handleIssueCreate(ctx, req)
	case "issues.update":
		return p.handleIssueUpdate(ctx, req)
	case "issues.upload":
		return p.handleIssueUpload(ctx, req)
	case "comments.get":
		return p.handleCommentGet(ctx, req)
	case "comments.prepare":
		return p.handleCommentPrepare(ctx, req)
	case "comments.approve":
		return p.handleCommentPublish(ctx, req, true)
	case "comments.reconcile":
		return p.handleCommentPublish(ctx, req, false)
	case "comments.discard":
		return p.handleCommentDiscard(ctx, req)
	case "replies.settings.get":
		return p.handleReplySettings(ctx, req, false)
	case "replies.settings.save":
		return p.handleReplySettings(ctx, req, true)
	case "replies.request":
		return p.handleReplyRequest(ctx, req)
	case "link.get":
		return p.handleLinkGet(ctx, req)
	case "link.set":
		return p.handleLinkSet(ctx, req)
	case "link.unset":
		return p.handleLinkUnset(ctx, req)
	case "link.set_status":
		return p.handleLinkSetStatus(ctx, req)
	case "watches.list":
		return p.handleWatchesList(ctx, req)
	case "watches.filter_options":
		return p.handleWatchFilterOptions(ctx, req)
	case "watches.create":
		return p.handleWatchesCreate(ctx, req)
	case "watches.update":
		return p.handleWatchesUpdate(ctx, req)
	case "watches.delete":
		return p.handleWatchesDelete(ctx, req)
	case "watches.poll":
		return p.handleWatchesPoll(ctx, req)
	default:
		return nil, fmt.Errorf("redmine: unknown action %q", req.ActionKey)
	}
}

type integrationEnabledResponse struct {
	Enabled bool `json:"enabled"`
}

func (p *redminePlugin) handleIntegrationEnabledGet(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	enabled, err := p.connectionSvc.GetEnabled(ctx, req.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return jsonResponse(integrationEnabledResponse{Enabled: enabled})
}

func (p *redminePlugin) handleIntegrationEnabledSave(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	body, err := decodeBody[integrationEnabledResponse](req)
	if err != nil {
		return nil, err
	}
	if err := p.connectionSvc.SetEnabled(ctx, req.Context.WorkspaceID, body.Enabled); err != nil {
		return nil, err
	}
	if body.Enabled {
		// Resume persisted links promptly after the workspace switch is
		// re-enabled instead of waiting for the normal poll cadence.
		p.requestPoll()
	}
	return jsonResponse(body)
}

func jsonResponse(v any) (*pluginsdk.PluginActionResponse, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("redmine: encoding action response: %w", err)
	}
	return &pluginsdk.PluginActionResponse{Body: body}, nil
}

func decodeBody[T any](req *pluginsdk.PluginActionRequest) (T, error) {
	var out T
	if len(req.Body) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(req.Body, &out); err != nil {
		return out, fmt.Errorf("redmine: decoding action %q body: %w", req.ActionKey, err)
	}
	return out, nil
}

// errorResponse reports a distinct, plugin-owned action error rather than a
// bare host-level failure — the spec's requirement that a Redmine
// credential rejection never surfaces as a generic 401 to the frontend (see
// spec "Failure modes").
type actionErrorResponse struct {
	Error string `json:"error"`
	Kind  string `json:"kind"`
}

func classifiedErrorResponse(err error) (*pluginsdk.PluginActionResponse, error) {
	kind := "unexpected"
	if apiErr, ok := asAPIError(err); ok {
		kind = string(apiErr.Kind)
	}
	body, marshalErr := json.Marshal(actionErrorResponse{Error: err.Error(), Kind: kind})
	if marshalErr != nil {
		return nil, marshalErr
	}
	return &pluginsdk.PluginActionResponse{Body: body, Status: 200}, nil
}

// --- connection.* -----------------------------------------------------

type connectionResponse struct {
	State     string `json:"state"`
	BaseURL   string `json:"base_url,omitempty"`
	LastOK    string `json:"last_ok,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

func (p *redminePlugin) handleConnectionGet(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	record, found, err := p.connectionSvc.Get(ctx, req.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !found {
		return jsonResponse(connectionResponse{State: "disconnected"})
	}
	return jsonResponse(connectionResponse{
		State: string(record.State), BaseURL: record.BaseURL, LastOK: record.LastOK, LastError: record.LastError,
	})
}

type connectionSaveRequest struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

func (p *redminePlugin) handleConnectionSave(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	body, err := decodeBody[connectionSaveRequest](req)
	if err != nil {
		return nil, err
	}
	var record *connection.Record
	if body.APIKey == "" {
		record, err = p.connectionSvc.ConnectWithExistingKey(ctx, req.Context.WorkspaceID, body.BaseURL)
	} else {
		record, err = p.connectionSvc.Connect(ctx, req.Context.WorkspaceID, body.BaseURL, body.APIKey)
	}
	if err != nil {
		return classifiedErrorResponse(err)
	}
	return jsonResponse(connectionResponse{State: string(record.State), BaseURL: record.BaseURL, LastOK: record.LastOK})
}

// handleConnectionDisconnect deletes every watch for the workspace first
// (cascading their created task trees via PluginOwnedTaskTrees) before
// removing the connection itself, so deleting a connection never leaves
// orphaned watcher tasks behind (spec "Failure modes").
func (p *redminePlugin) handleConnectionDisconnect(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if err := p.clearWorkspace(ctx, req.Context.WorkspaceID, false); err != nil {
		return nil, err
	}
	return jsonResponse(map[string]bool{"disconnected": true})
}

// --- projects.* ---------------------------------------------------------

type projectsListResponse struct {
	Projects    []redmineProject `json:"projects"`
	SelectedIDs []int            `json:"selected_ids"`
}

type redmineProject struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Identifier string `json:"identifier"`
}

func (p *redminePlugin) handleProjectsList(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	client, err := p.connectionSvc.Client(ctx, req.Context.WorkspaceID)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	live, err := p.projectsSvc.ListLive(ctx, client)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	selected, err := p.projectsSvc.GetSelection(ctx, req.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]redmineProject, len(live))
	for i, proj := range live {
		out[i] = redmineProject{ID: proj.ID, Name: proj.Name, Identifier: proj.Identifier}
	}
	return jsonResponse(projectsListResponse{Projects: out, SelectedIDs: selected})
}

type projectsSaveRequest struct {
	ProjectIDs []int `json:"project_ids"`
}

func (p *redminePlugin) handleProjectsSave(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	body, err := decodeBody[projectsSaveRequest](req)
	if err != nil {
		return nil, err
	}
	client, err := p.connectionSvc.Client(ctx, req.Context.WorkspaceID)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	live, err := p.projectsSvc.ListLive(ctx, client)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	visible := make(map[int]bool, len(live))
	for _, project := range live {
		visible[project.ID] = true
	}
	selected := make([]int, 0, len(body.ProjectIDs))
	seen := make(map[int]bool, len(body.ProjectIDs))
	for _, id := range body.ProjectIDs {
		if id <= 0 || !visible[id] {
			return classifiedErrorResponse(fmt.Errorf("redmine: selected project %d is not visible", id))
		}
		if !seen[id] {
			selected = append(selected, id)
			seen[id] = true
		}
	}
	sort.Ints(selected)
	if err := p.projectsSvc.SaveSelection(ctx, req.Context.WorkspaceID, selected); err != nil {
		return nil, err
	}
	return jsonResponse(map[string]bool{"saved": true})
}

// --- fieldmapping.* -------------------------------------------------------

type fieldMappingGetResponse struct {
	WorkflowID          string                         `json:"workflow_id"`
	Statuses            []fieldmapping.StatusMapping   `json:"statuses"`
	Priorities          []fieldmapping.PriorityMapping `json:"priorities"`
	LiveStatuses        []redmineNamedRef              `json:"live_statuses"`
	LivePriorities      []redmineNamedRef              `json:"live_priorities"`
	CustomFields        []fieldmapping.CustomField     `json:"custom_fields"`
	CustomFieldsDerived bool                           `json:"custom_fields_derived"`
}

type redmineNamedRef struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	IsClosed bool   `json:"is_closed,omitempty"`
}

func (p *redminePlugin) handleFieldMappingGet(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	client, err := p.connectionSvc.Client(ctx, req.Context.WorkspaceID)
	if err != nil {
		return classifiedErrorResponse(err)
	}

	liveStatuses, err := client.ListIssueStatuses(ctx)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	livePriorities, err := client.ListIssuePriorities(ctx)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	customFields, derived, err := p.resolveCustomFields(ctx, client)
	if err != nil {
		return classifiedErrorResponse(err)
	}

	mapping, _, err := p.fieldmappingSvc.Get(ctx, req.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}

	return jsonResponse(fieldMappingGetResponse{
		WorkflowID: mapping.WorkflowID,
		Statuses:   mapping.Statuses, Priorities: mapping.Priorities,
		LiveStatuses:        toNamedRefs(liveStatuses),
		LivePriorities:      toPriorityRefs(livePriorities),
		CustomFields:        customFields,
		CustomFieldsDerived: derived,
	})
}

// resolveCustomFields fetches /custom_fields.json (admin key); on a
// non-admin 403 it derives fields from the union of custom fields observed
// on a recent page of issues instead of treating the 403 as an error (spec
// "Field mapping").
func (p *redminePlugin) resolveCustomFields(ctx context.Context, client *redmineclient.Client) ([]fieldmapping.CustomField, bool, error) {
	live, err := client.ListCustomFields(ctx)
	if err == nil {
		out := make([]fieldmapping.CustomField, len(live))
		for i, f := range live {
			out[i] = fieldmapping.CustomField{ID: f.ID, Name: f.Name}
		}
		return out, false, nil
	}
	apiErr, ok := asAPIError(err)
	if !ok || apiErr.Kind != redmineclient.ErrKindPermissionDenied {
		return nil, false, err
	}

	issuesSvc := issues.New(client)
	result, err := issuesSvc.ListIssues(ctx, issues.ListIssuesParams{Limit: 25})
	if err != nil {
		return nil, false, err
	}
	perIssue := make([][]fieldmapping.CustomField, len(result.Issues))
	for i, issue := range result.Issues {
		fields := make([]fieldmapping.CustomField, len(issue.CustomFields))
		for j, f := range issue.CustomFields {
			fields[j] = fieldmapping.CustomField{ID: f.ID, Name: f.Name}
		}
		perIssue[i] = fields
	}
	return fieldmapping.DeriveCustomFieldsFromIssues(perIssue), true, nil
}

func (p *redminePlugin) handleFieldMappingSave(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	body, err := decodeBody[fieldmapping.Mapping](req)
	if err != nil {
		return nil, err
	}
	if err := p.validateMappingWorkflow(ctx, req.Context.WorkspaceID, body); err != nil {
		return classifiedErrorResponse(err)
	}
	client, err := p.connectionSvc.Client(ctx, req.Context.WorkspaceID)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	if err := p.normalizeAndValidateMapping(ctx, client, &body); err != nil {
		return classifiedErrorResponse(err)
	}
	if err := p.fieldmappingSvc.Save(ctx, req.Context.WorkspaceID, body); err != nil {
		return nil, err
	}
	return jsonResponse(map[string]bool{"saved": true})
}

func (p *redminePlugin) normalizeAndValidateMapping(ctx context.Context, client *redmineclient.Client, mapping *fieldmapping.Mapping) error {
	statuses, err := client.ListIssueStatuses(ctx)
	if err != nil {
		return err
	}
	priorities, err := client.ListIssuePriorities(ctx)
	if err != nil {
		return err
	}
	statusByID := make(map[int]redmineclient.IssueStatus, len(statuses))
	for _, value := range statuses {
		statusByID[value.ID] = value
	}
	priorityByID := make(map[int]redmineclient.Priority, len(priorities))
	for _, value := range priorities {
		priorityByID[value.ID] = value
	}
	seenStatuses, seenPriorities := map[int]bool{}, map[int]bool{}
	for i := range mapping.Statuses {
		item := &mapping.Statuses[i]
		live, ok := statusByID[item.RedmineStatusID]
		if item.RedmineStatusID <= 0 || !ok || seenStatuses[item.RedmineStatusID] {
			return fmt.Errorf("redmine: status mapping id %d is invalid or duplicated", item.RedmineStatusID)
		}
		seenStatuses[item.RedmineStatusID] = true
		item.RedmineName = live.Name
		item.IsClosed = live.IsClosed
	}
	for i := range mapping.Priorities {
		item := &mapping.Priorities[i]
		live, ok := priorityByID[item.RedminePriorityID]
		if item.RedminePriorityID <= 0 || !ok || seenPriorities[item.RedminePriorityID] {
			return fmt.Errorf("redmine: priority mapping id %d is invalid or duplicated", item.RedminePriorityID)
		}
		if item.TaskPriority != "" && item.TaskPriority != "critical" && item.TaskPriority != "high" && item.TaskPriority != "medium" && item.TaskPriority != "low" {
			return fmt.Errorf("redmine: task priority %q is invalid", item.TaskPriority)
		}
		seenPriorities[item.RedminePriorityID] = true
		item.RedmineName = live.Name
	}
	return nil
}

func (p *redminePlugin) validateMappingWorkflow(ctx context.Context, workspaceID string, mapping fieldmapping.Mapping) error {
	if mapping.WorkflowID == "" {
		return fmt.Errorf("redmine: workflow_id is required")
	}
	workflows, err := listWorkspaceWorkflows(ctx, p.Host().Workflows(), workspaceID)
	if err != nil {
		return err
	}
	found := false
	for _, workflow := range workflows {
		if workflow.ID == mapping.WorkflowID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("redmine: workflow %s does not belong to this workspace", mapping.WorkflowID)
	}
	steps, err := p.Host().Workflows().ListSteps(ctx, mapping.WorkflowID)
	if err != nil {
		return err
	}
	valid := make(map[string]bool, len(steps))
	for _, step := range steps {
		valid[step.ID] = true
	}
	for _, status := range mapping.Statuses {
		if status.WorkflowStepID != "" && !valid[status.WorkflowStepID] {
			return fmt.Errorf("redmine: workflow step %s is not in workflow %s", status.WorkflowStepID, mapping.WorkflowID)
		}
	}
	return nil
}

// --- syncoptions.* --------------------------------------------------------

func (p *redminePlugin) handleSyncOptionsGet(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	opts, err := p.syncSvc.GetOptions(ctx, req.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return jsonResponse(map[string]bool{"auto_status_writeback": opts.AutoStatusWriteback, "sync_title_description": opts.SyncTitleDescription})
}

func (p *redminePlugin) handleSyncOptionsSave(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	body, err := decodeBody[syncOptionsSaveRequest](req)
	if err != nil {
		return nil, err
	}
	if err := p.syncSvc.SaveOptions(ctx, req.Context.WorkspaceID, body.toOptions()); err != nil {
		return nil, err
	}
	return jsonResponse(map[string]bool{"saved": true})
}

type syncOptionsSaveRequest struct {
	AutoStatusWriteback  bool `json:"auto_status_writeback"`
	SyncTitleDescription bool `json:"sync_title_description"`
}

func (r syncOptionsSaveRequest) toOptions() redminesync.Options {
	return redminesync.Options{AutoStatusWriteback: r.AutoStatusWriteback, SyncTitleDescription: r.SyncTitleDescription}
}

func toNamedRefs(statuses []redmineclient.IssueStatus) []redmineNamedRef {
	out := make([]redmineNamedRef, len(statuses))
	for i, s := range statuses {
		out[i] = redmineNamedRef{ID: s.ID, Name: s.Name, IsClosed: s.IsClosed}
	}
	return out
}

func toTrackerRefs(trackers []redmineclient.Tracker) []redmineNamedRef {
	out := make([]redmineNamedRef, len(trackers))
	for i, t := range trackers {
		out[i] = redmineNamedRef{ID: t.ID, Name: t.Name}
	}
	return out
}

func toPriorityRefs(priorities []redmineclient.Priority) []redmineNamedRef {
	out := make([]redmineNamedRef, len(priorities))
	for i, priority := range priorities {
		out[i] = redmineNamedRef{ID: priority.ID, Name: priority.Name}
	}
	return out
}
