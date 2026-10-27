package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
	"kandev-plugin-redmine/internal/fieldmapping"
	"kandev-plugin-redmine/internal/issues"
	"kandev-plugin-redmine/internal/redmineclient"
	"kandev-plugin-redmine/internal/tasklink"
)

func routedMapping() fieldmapping.Mapping {
	return fieldmapping.Mapping{WorkflowID: "default", Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 3, WorkflowStepID: "default-working"}}, TrackerRoutes: []fieldmapping.TrackerRoute{
		{RedmineTrackerID: 8, WorkflowID: "bugs", WorkflowStepID: "diagnosis", Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 3, WorkflowStepID: "diagnosis"}, {RedmineStatusID: 5, WorkflowStepID: "done"}}},
		{RedmineTrackerID: 9, WorkflowID: "bugs", WorkflowStepID: "plan", Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 3, WorkflowStepID: "plan"}}},
	}}
}

func TestTrackerStatusesUseTheirOwnMapAndDoNotResetInternalProgress(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		tracker, status, observed int
		workflow, step, want      string
	}{
		{"first observation", 8, 3, 0, "bugs", "plan", "diagnosis"},
		{"unchanged working status", 8, 3, 3, "bugs", "plan", ""},
		{"external status changed", 8, 5, 3, "bugs", "plan", "done"},
		{"second tracker in same workflow", 9, 3, 0, "bugs", "diagnosis", "plan"},
		{"default tracker", 4, 3, 3, "default", "backlog", "default-working"},
		{"unmapped tracker never uses foreign default map", 4, 3, 0, "bugs", "plan", ""},
		{"human communication gate", 8, 5, 3, "bugs", "client-reply", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := buildInboundPlan("task", pluginsdk.Task{WorkflowID: tc.workflow, WorkflowStepID: tc.step}, tasklink.Link{ObservedStatusID: tc.observed, ObservedTrackerID: tc.tracker}, issues.Issue{TrackerID: tc.tracker, StatusID: tc.status, Subject: "Current subject"}, routedMapping(), Options{SyncTitleDescription: true, PreserveWorkflowStepIDs: []string{"client-reply"}})
			require.Equal(t, tc.want, plan.workflowStepID)
			require.NotNil(t, plan.update.Title)
		})
	}
}

func TestInboundRecordsRoutedStatusEvenAtHumanGate(t *testing.T) {
	host := newFakeHost()
	host.task = pluginsdk.Task{WorkflowID: "bugs", WorkflowStepID: "client-reply"}
	links := tasklink.New(host)
	require.NoError(t, links.Set(context.Background(), "task", "workspace", 42, "url"))
	svc := New(host, links)
	issue := issues.Issue{ID: 42, TrackerID: 8, StatusID: 5}
	require.NoError(t, svc.applyInbound(context.Background(), "workspace", issue, routedMapping(), Options{PreserveWorkflowStepIDs: []string{"client-reply"}}))
	host.task.WorkflowStepID = "plan"
	require.NoError(t, svc.applyInbound(context.Background(), "workspace", issue, routedMapping(), Options{}))
	require.Empty(t, host.moveCalls(), "a deferred external status must not skip the human-controlled continuation later")
}

func TestWritebackSelectsLiveTrackerAndRejectsStaleMoveOrAmbiguousStatus(t *testing.T) {
	host := newFakeHost()
	host.task = pluginsdk.Task{WorkflowID: "bugs", WorkflowStepID: "plan"}
	links := tasklink.New(host)
	require.NoError(t, links.Set(context.Background(), "task", "workspace", 42, "url"))
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			return
		}
		out.Write([]byte(`{"issue":{"id":42,"tracker":{"id":9},"status":{"id":1}}}`))
	}))
	defer srv.Close()
	svc := New(host, links)
	issuesSvc := issues.New(redmineclient.New(srv.URL, "key", srv.Client()))
	opts := Options{AutoStatusWriteback: true}
	require.NoError(t, svc.PushWriteback(context.Background(), "task", "old-step", routedMapping(), issuesSvc, opts))
	require.Zero(t, puts)
	require.NoError(t, svc.PushWriteback(context.Background(), "task", "plan", routedMapping(), issuesSvc, opts))
	require.Equal(t, 1, puts)
	mapping := routedMapping()
	mapping.TrackerRoutes[1].Statuses = append(mapping.TrackerRoutes[1].Statuses, fieldmapping.StatusMapping{RedmineStatusID: 7, WorkflowStepID: "plan"})
	require.ErrorContains(t, svc.PushWriteback(context.Background(), "task", "plan", mapping, issuesSvc, opts), "ambiguous")
	require.Equal(t, 1, puts)
}
