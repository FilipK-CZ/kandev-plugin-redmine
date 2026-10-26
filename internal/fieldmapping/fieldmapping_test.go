package fieldmapping

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestService_SaveAndGet_RoundTrips(t *testing.T) {
	svc := New(newFakeHost())
	mapping := Mapping{
		TrackerRoutes: []TrackerRoute{{RedmineTrackerID: 8, WorkflowID: "bugs", WorkflowStepID: "diagnosis"}},
		Statuses: []StatusMapping{
			{RedmineStatusID: 2, RedmineName: "Shipped", IsClosed: true, WorkflowStepID: "step-done"},
		},
		Priorities: []PriorityMapping{
			{RedminePriorityID: 1, RedmineName: "Urgent", TaskPriority: "critical"},
		},
	}

	require.NoError(t, svc.Save(context.Background(), "ws-1", mapping))

	got, found, err := svc.Get(context.Background(), "ws-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, mapping, got)
}

func TestGet_NoneSaved_ReturnsNotFound(t *testing.T) {
	svc := New(newFakeHost())
	_, found, err := svc.Get(context.Background(), "ws-1")
	require.NoError(t, err)
	require.False(t, found)
}

func TestGet_CapturesIsClosedFlagPerStatus(t *testing.T) {
	svc := New(newFakeHost())
	mapping := Mapping{Statuses: []StatusMapping{
		{RedmineStatusID: 1, RedmineName: "Triage", IsClosed: false, WorkflowStepID: "step-a"},
		{RedmineStatusID: 2, RedmineName: "Archived", IsClosed: true, WorkflowStepID: "step-b"},
	}}
	require.NoError(t, svc.Save(context.Background(), "ws-1", mapping))

	got, found, err := svc.Get(context.Background(), "ws-1")
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, got.Statuses[0].IsClosed)
	require.True(t, got.Statuses[1].IsClosed)
}

func TestDeriveCustomFieldsFromIssues_UnionsAndDedupsByID(t *testing.T) {
	fields := DeriveCustomFieldsFromIssues([][]CustomField{
		{{ID: 1, Name: "Severity"}, {ID: 2, Name: "Team"}},
		{{ID: 2, Name: "Team"}, {ID: 3, Name: "Component"}},
	})
	require.Len(t, fields, 3)

	ids := make([]int, len(fields))
	for i, f := range fields {
		ids[i] = f.ID
	}
	require.ElementsMatch(t, []int{1, 2, 3}, ids)
}

func TestDeriveCustomFieldsFromIssues_PreservesKnownNameWhenLaterObservationOmitsIt(t *testing.T) {
	fields := DeriveCustomFieldsFromIssues([][]CustomField{
		{{ID: 7, Name: "Customer tier"}},
		{{ID: 7, Name: ""}},
	})

	require.Equal(t, []CustomField{{ID: 7, Name: "Customer tier"}}, fields)
}

func TestDeriveCustomFieldsFromIssues_NoIssues_ReturnsEmpty(t *testing.T) {
	fields := DeriveCustomFieldsFromIssues(nil)
	require.Empty(t, fields)
}

func TestWorkflowStepForStatus_ResolvesInboundDirection(t *testing.T) {
	m := Mapping{Statuses: []StatusMapping{
		{RedmineStatusID: 1, WorkflowStepID: "step-backlog"},
		{RedmineStatusID: 2, WorkflowStepID: "step-done"},
	}}
	step, ok := m.WorkflowStepForStatus(2)
	require.True(t, ok)
	require.Equal(t, "step-done", step)

	_, ok = m.WorkflowStepForStatus(99)
	require.False(t, ok)
}

func TestStatusForWorkflowStep_ResolvesOutboundDirection(t *testing.T) {
	m := Mapping{Statuses: []StatusMapping{
		{RedmineStatusID: 1, WorkflowStepID: "step-backlog"},
		{RedmineStatusID: 2, WorkflowStepID: "step-done"},
	}}
	statusID, ok := m.StatusForWorkflowStep("step-done")
	require.True(t, ok)
	require.Equal(t, 2, statusID)

	_, ok = m.StatusForWorkflowStep("step-unmapped")
	require.False(t, ok)
}

func TestMappingIgnoresLegacyTrackerLabelState(t *testing.T) {
	mapping := mappingFromMap(map[string]any{
		"workflow_id": "wf-1",
		"trackers": []any{map[string]any{
			"redmine_tracker_id": float64(3),
			"redmine_name":       "Bug",
			"task_label":         "bug",
		}},
		"priorities": []any{map[string]any{
			"redmine_priority_id": float64(4),
			"redmine_name":        "High",
			"task_priority":       "high",
		}},
	})

	require.Equal(t, "wf-1", mapping.WorkflowID)
	require.Len(t, mapping.Priorities, 1)
	require.NotContains(t, mapping.toMap(), "trackers")
}

func TestTaskPriorityForRedminePriority_ResolvesConfiguredNonEmptyMapping(t *testing.T) {
	mapping := Mapping{Priorities: []PriorityMapping{
		{RedminePriorityID: 4, TaskPriority: "high"},
		{RedminePriorityID: 5, TaskPriority: ""},
	}}

	priority, ok := mapping.TaskPriorityForRedminePriority(4)
	require.True(t, ok)
	require.Equal(t, "high", priority)
	_, ok = mapping.TaskPriorityForRedminePriority(5)
	require.False(t, ok)
	_, ok = mapping.TaskPriorityForRedminePriority(99)
	require.False(t, ok)
}
