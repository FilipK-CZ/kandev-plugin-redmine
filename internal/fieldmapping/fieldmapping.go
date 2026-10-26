// Package fieldmapping persists per-workspace status/priority mapping from
// live Redmine field data to Kandev concepts, and derives
// custom-field definitions from observed issues when /custom_fields.json is
// unavailable to a non-admin API key. No status or priority name is ever
// hardcoded here — every mapping entry is built from data the caller fetched
// live via internal/redmineclient.
package fieldmapping

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// StatusMapping maps one live Redmine issue status to a Kandev workflow
// step, capturing IsClosed so a closed-status transition can be recognized
// without a second live lookup.
type StatusMapping struct {
	RedmineStatusID int    `json:"redmine_status_id"`
	RedmineName     string `json:"redmine_name"`
	IsClosed        bool   `json:"is_closed"`
	WorkflowStepID  string `json:"workflow_step_id"`
}

// PriorityMapping maps one live Redmine priority to a Kandev task priority
// (critical|high|medium|low).
type PriorityMapping struct {
	RedminePriorityID int    `json:"redmine_priority_id"`
	RedmineName       string `json:"redmine_name"`
	TaskPriority      string `json:"task_priority"`
}

// CustomField is one Redmine custom field definition, either fetched live
// from /custom_fields.json (admin key) or derived from issues observed on
// the wire (DeriveCustomFieldsFromIssues).
type CustomField struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Mapping is the full persisted per-workspace field mapping.
type Mapping struct {
	WorkflowID    string            `json:"workflow_id"`
	Statuses      []StatusMapping   `json:"statuses"`
	Priorities    []PriorityMapping `json:"priorities"`
	TrackerRoutes []TrackerRoute    `json:"tracker_routes,omitempty"`
}

// TrackerRoute changes placement only for newly imported watcher tasks.
type TrackerRoute struct {
	RedmineTrackerID int                    `json:"redmine_tracker_id"`
	WorkflowID       string                 `json:"workflow_id"`
	WorkflowStepID   string                 `json:"workflow_step_id"`
	Statuses         []StatusMapping        `json:"statuses,omitempty"`
	AutomaticIntake  *AutomaticIntakePolicy `json:"automatic_intake,omitempty"`
}

// AutomaticIntakePolicy permits one status-only claim, independently of write-back.
type AutomaticIntakePolicy struct {
	FromStatusID    int `json:"from_status_id"`
	WorkingStatusID int `json:"working_status_id"`
}

func (r TrackerRoute) IntakeEntry(steps []pluginsdk.WorkflowStep) (pluginsdk.WorkflowStep, error) {
	if r.AutomaticIntake == nil || r.AutomaticIntake.FromStatusID <= 0 || r.AutomaticIntake.WorkingStatusID <= 0 || r.AutomaticIntake.FromStatusID == r.AutomaticIntake.WorkingStatusID {
		return pluginsdk.WorkflowStep{}, fmt.Errorf("redmine: automatic intake needs distinct source and working statuses")
	}
	for _, item := range r.Statuses {
		if (item.RedmineStatusID == r.AutomaticIntake.FromStatusID || item.RedmineStatusID == r.AutomaticIntake.WorkingStatusID) && item.WorkflowStepID != r.WorkflowStepID {
			return pluginsdk.WorkflowStep{}, fmt.Errorf("redmine: intake source and working statuses must stay unmapped or map to the selected entry step")
		}
	}
	for _, step := range steps {
		if step.ID == r.WorkflowStepID && step.AgentProfileID != "" {
			for _, action := range step.OnEnterActionTypes {
				if action == "auto_start_agent" {
					return step, nil
				}
			}
		}
	}
	return pluginsdk.WorkflowStep{}, fmt.Errorf("redmine: automatic intake requires an entry step with a fixed profile and native auto-start enabled")
}

// StatusesFor keeps tracker overrides isolated from the default workflow map.
func (m Mapping) StatusesFor(trackerID int, workflowID string) []StatusMapping {
	for _, route := range m.TrackerRoutes {
		if route.RedmineTrackerID == trackerID && route.WorkflowID == workflowID {
			return route.Statuses
		}
	}
	if workflowID == m.WorkflowID {
		return m.Statuses
	}
	return nil
}

// WorkflowStepForStatus resolves the inbound direction: a Redmine status ID
// to the Kandev workflow step it maps to.
func (m Mapping) WorkflowStepForStatus(redmineStatusID int) (string, bool) {
	for _, s := range m.Statuses {
		if s.RedmineStatusID == redmineStatusID {
			return s.WorkflowStepID, s.WorkflowStepID != ""
		}
	}
	return "", false
}

// StatusForWorkflowStep resolves the outbound (write-back) direction: a
// Kandev workflow step to the Redmine status ID it maps to.
func (m Mapping) StatusForWorkflowStep(workflowStepID string) (int, bool) {
	for _, s := range m.Statuses {
		if s.WorkflowStepID == workflowStepID {
			return s.RedmineStatusID, true
		}
	}
	return 0, false
}

// TaskPriorityForRedminePriority resolves the inbound direction: a Redmine
// priority ID to the Kandev task priority it maps to
// (critical|high|medium|low). An empty TaskPriority entry is reported as
// unmapped.
func (m Mapping) TaskPriorityForRedminePriority(redminePriorityID int) (string, bool) {
	for _, p := range m.Priorities {
		if p.RedminePriorityID == redminePriorityID {
			return p.TaskPriority, p.TaskPriority != ""
		}
	}
	return "", false
}

const (
	stateScope = "workspace"
	stateKey   = "field_mapping"
)

type Service struct {
	host pluginsdk.Host
}

func New(host pluginsdk.Host) *Service {
	return &Service{host: host}
}

func (s *Service) Save(ctx context.Context, workspaceID string, mapping Mapping) error {
	if err := s.host.SetState(ctx, stateScope, workspaceID, stateKey, mapping.toMap()); err != nil {
		return fmt.Errorf("fieldmapping: saving: %w", err)
	}
	return nil
}

func (s *Service) Get(ctx context.Context, workspaceID string) (Mapping, bool, error) {
	value, found, err := s.host.GetState(ctx, stateScope, workspaceID, stateKey)
	if err != nil {
		return Mapping{}, false, fmt.Errorf("fieldmapping: reading: %w", err)
	}
	if !found {
		return Mapping{}, false, nil
	}
	return mappingFromMap(value), true, nil
}

func (s *Service) Clear(ctx context.Context, workspaceID string) error {
	if err := s.host.DeleteState(ctx, stateScope, workspaceID, stateKey); err != nil {
		return fmt.Errorf("fieldmapping: clearing: %w", err)
	}
	return nil
}

// DeriveCustomFieldsFromIssues unions the custom fields observed across a
// set of already-fetched issues, deduplicated by field ID — the fallback the
// spec requires when /custom_fields.json 403s for a non-admin API key,
// surfaced to the UI layer with a "derived from recent issues" note (task
// 08) rather than treated as an error.
func DeriveCustomFieldsFromIssues(issuesFields [][]CustomField) []CustomField {
	seen := make(map[int]CustomField)
	order := make([]int, 0)
	for _, fields := range issuesFields {
		for _, f := range fields {
			previous, ok := seen[f.ID]
			if !ok {
				order = append(order, f.ID)
			}
			// Issue-list responses can omit a custom-field name on a later
			// observation. Keep the usable name already observed for the same
			// ID rather than replacing it with an unnamed field in the fallback.
			if f.Name == "" && previous.Name != "" {
				f.Name = previous.Name
			}
			seen[f.ID] = f
		}
	}
	out := make([]CustomField, 0, len(order))
	for _, id := range order {
		out = append(out, seen[id])
	}
	return out
}

func (m Mapping) toMap() map[string]any {
	routes := make([]any, len(m.TrackerRoutes))
	for i, route := range m.TrackerRoutes {
		encoded, _ := json.Marshal(route)
		var value map[string]any
		_ = json.Unmarshal(encoded, &value)
		routes[i] = value
	}
	statuses := make([]any, len(m.Statuses))
	for i, s := range m.Statuses {
		statuses[i] = map[string]any{
			"redmine_status_id": s.RedmineStatusID,
			"redmine_name":      s.RedmineName,
			"is_closed":         s.IsClosed,
			"workflow_step_id":  s.WorkflowStepID,
		}
	}
	priorities := make([]any, len(m.Priorities))
	for i, p := range m.Priorities {
		priorities[i] = map[string]any{
			"redmine_priority_id": p.RedminePriorityID,
			"redmine_name":        p.RedmineName,
			"task_priority":       p.TaskPriority,
		}
	}
	return map[string]any{
		"workflow_id":    m.WorkflowID,
		"statuses":       statuses,
		"priorities":     priorities,
		"tracker_routes": routes,
	}
}

func mappingFromMap(m map[string]any) Mapping {
	out := Mapping{WorkflowID: asString(m["workflow_id"])}
	for _, raw := range asSlice(m["tracker_routes"]) {
		if row, ok := raw.(map[string]any); ok {
			encoded, _ := json.Marshal(row)
			var route TrackerRoute
			_ = json.Unmarshal(encoded, &route)
			out.TrackerRoutes = append(out.TrackerRoutes, route)
		}
	}
	for _, raw := range asSlice(m["statuses"]) {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out.Statuses = append(out.Statuses, StatusMapping{
			RedmineStatusID: asInt(row["redmine_status_id"]),
			RedmineName:     asString(row["redmine_name"]),
			IsClosed:        asBool(row["is_closed"]),
			WorkflowStepID:  asString(row["workflow_step_id"]),
		})
	}
	for _, raw := range asSlice(m["priorities"]) {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out.Priorities = append(out.Priorities, PriorityMapping{
			RedminePriorityID: asInt(row["redmine_priority_id"]),
			RedmineName:       asString(row["redmine_name"]),
			TaskPriority:      asString(row["task_priority"]),
		})
	}
	return out
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func asInt(v any) int {
	f, _ := v.(float64)
	return int(f)
}
