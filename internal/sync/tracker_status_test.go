package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
	"kandev-plugin-redmine/internal/fieldmapping"
	"kandev-plugin-redmine/internal/issues"
	"kandev-plugin-redmine/internal/redmineclient"
	"kandev-plugin-redmine/internal/tasklink"
)

func TestTrackerWritebackFollowsLiveStepsAndTracker(t *testing.T) {
	host := newFakeHost()
	host.task = pluginsdk.Task{WorkflowID: "bugs", WorkflowStepID: "implementation"}
	links := tasklink.New(host)
	require.NoError(t, links.Set(context.Background(), "task", "workspace", 42, "url"))
	var currentStatus, currentTracker, puts atomic.Int64
	currentStatus.Store(1)
	currentTracker.Store(8)
	srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var body map[string]map[string]int
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Len(t, body, 1)
			require.Len(t, body["issue"], 1)
			currentStatus.Store(int64(body["issue"]["status_id"]))
			puts.Add(1)
			return
		}
		fmt.Fprintf(out, `{"issue":{"id":42,"tracker":{"id":%d},"status":{"id":%d}}}`, currentTracker.Load(), currentStatus.Load())
	}))
	defer srv.Close()
	mapping := routedMapping()
	mapping.TrackerRoutes[0].AutoStatusWriteback = true
	mapping.TrackerRoutes[0].Statuses = []fieldmapping.StatusMapping{{RedmineStatusID: 3, WorkflowStepID: "implementation"}, {RedmineStatusID: 5, WorkflowStepID: "review"}}
	mapping.TrackerRoutes[1].Statuses = []fieldmapping.StatusMapping{{RedmineStatusID: 7, WorkflowStepID: "review"}}
	svc := New(host, links)
	issuesSvc := issues.New(redmineclient.New(srv.URL, "key", srv.Client()))
	for _, tc := range []struct {
		step         string
		status, puts int64
	}{{"implementation", 3, 1}, {"review", 5, 2}, {"client-reply", 5, 2}, {"implementation", 3, 3}} {
		host.task.WorkflowStepID = tc.step
		require.NoError(t, svc.PushWriteback(context.Background(), "task", tc.step, mapping, issuesSvc, Options{}))
		require.Equal(t, tc.status, currentStatus.Load())
		require.Equal(t, tc.puts, puts.Load())
	}
	currentTracker.Store(9)
	host.task.WorkflowStepID = "review"
	require.NoError(t, svc.PushWriteback(context.Background(), "task", "review", mapping, issuesSvc, Options{}))
	require.Equal(t, int64(3), puts.Load(), "the live issue's new tracker is not opted in")
	require.NoError(t, svc.PushWriteback(context.Background(), "task", "review", mapping, issuesSvc, Options{AutoStatusWriteback: true}))
	require.Equal(t, int64(7), currentStatus.Load(), "global behavior still uses the live tracker's own mapping")
	require.Equal(t, int64(4), puts.Load())
}

func TestTrackerWritebackWorkingStatusDoesNotSkipIntakeOrResetProgress(t *testing.T) {
	mapping := routedMapping()
	mapping.TrackerRoutes[0].AutoStatusWriteback = true
	mapping.TrackerRoutes[0].Statuses = []fieldmapping.StatusMapping{{RedmineStatusID: 3, WorkflowStepID: "implementation"}}
	workingStatus := 3
	for _, step := range []string{"plan", "review"} {
		link := tasklink.Link{ObservedStatusID: workingStatus, ObservedTrackerID: 8, LastPushedStatusID: &workingStatus}
		plan := buildInboundPlan("task", pluginsdk.Task{WorkflowID: "bugs", WorkflowStepID: step}, link, issues.Issue{TrackerID: 8, StatusID: workingStatus}, mapping, Options{})
		require.Empty(t, plan.workflowStepID, "an unchanged working-status echo must not move the task to implementation")
		require.True(t, plan.consumeStatusEcho)
	}
}

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
