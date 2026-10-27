package main

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
	"kandev-plugin-redmine/internal/redmineclient"
	redminesync "kandev-plugin-redmine/internal/sync"
)

func TestTrackerWritebackOnNativeMoves(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		tracker                             int
		workflow, step, eventStep           string
		scoped, global, disabled, ambiguous bool
		wantPuts                            int64
	}{
		{name: "selected tracker with global off", tracker: 8, workflow: "bugs", step: "review", eventStep: "review", scoped: true, wantPuts: 1},
		{name: "legacy rule stays off", tracker: 8, workflow: "bugs", step: "review", eventStep: "review"},
		{name: "other tracker with same mapped step", tracker: 9, workflow: "bugs", step: "review", eventStep: "review", scoped: true},
		{name: "other workflow with same mapped step", tracker: 8, workflow: "default", step: "review", eventStep: "review", scoped: true},
		{name: "unmapped human step", tracker: 8, workflow: "bugs", step: "client-reply", eventStep: "client-reply", scoped: true},
		{name: "stale event", tracker: 8, workflow: "bugs", step: "review", eventStep: "implementation", scoped: true},
		{name: "disabled integration", tracker: 8, workflow: "bugs", step: "review", eventStep: "review", scoped: true, disabled: true},
		{name: "global option keeps existing behavior", tracker: 8, workflow: "bugs", step: "review", eventStep: "review", global: true, wantPuts: 1},
		{name: "ambiguous persisted map fails closed", tracker: 8, workflow: "bugs", step: "review", eventStep: "review", scoped: true, ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, host := newTestPlugin(t)
			host.tasks["task"] = &pluginsdk.Task{ID: "task", WorkspaceID: "ws-1", WorkflowID: tc.workflow, WorkflowStepID: tc.step}
			var puts, currentStatus atomic.Int64
			currentStatus.Store(1)
			srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/users/current.json":
					fmt.Fprint(out, `{"user":{"id":7,"login":"service"}}`)
				case r.URL.Path == "/projects.json":
					fmt.Fprint(out, `{"projects":[{"id":1,"name":"Project"}],"total_count":1}`)
				case r.Method == http.MethodGet && r.URL.Path == "/issues/42.json":
					fmt.Fprintf(out, `{"issue":{"id":42,"project":{"id":1},"tracker":{"id":%d},"status":{"id":%d},"subject":"Example"}}`, tc.tracker, currentStatus.Load())
				case r.Method == http.MethodPut && r.URL.Path == "/issues/42.json":
					var body map[string]map[string]int
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, map[string]map[string]int{"issue": {"status_id": 5}}, body, "write-back must change status only")
					puts.Add(1)
					currentStatus.Store(int64(body["issue"]["status_id"]))
				default:
					out.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			handle(t, p, "connection.save", "ws-1", "", map[string]any{"base_url": srv.URL, "api_key": "test-key"})
			require.NoError(t, p.projectsSvc.SaveSelection(context.Background(), "ws-1", []int{1}))
			handle(t, p, "link.set", "ws-1", "task", map[string]any{"reference": "#42"})
			mapping := fieldmapping.Mapping{WorkflowID: "default", Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 5, WorkflowStepID: "review"}}, TrackerRoutes: []fieldmapping.TrackerRoute{
				{RedmineTrackerID: 8, WorkflowID: "bugs", WorkflowStepID: "plan", AutoStatusWriteback: tc.scoped, Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 5, WorkflowStepID: "review"}}},
				{RedmineTrackerID: 9, WorkflowID: "bugs", WorkflowStepID: "plan", Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 5, WorkflowStepID: "review"}}},
			}}
			if tc.ambiguous {
				mapping.TrackerRoutes[0].Statuses = append(mapping.TrackerRoutes[0].Statuses, fieldmapping.StatusMapping{RedmineStatusID: 7, WorkflowStepID: "review"})
			}
			require.NoError(t, p.fieldmappingSvc.Save(context.Background(), "ws-1", mapping))
			require.NoError(t, p.syncSvc.SaveOptions(context.Background(), "ws-1", redminesync.Options{AutoStatusWriteback: tc.global}))
			if tc.disabled {
				require.NoError(t, p.connectionSvc.SetEnabled(context.Background(), "ws-1", false))
			}
			event := &pluginsdk.Event{EventType: "task.moved", EventID: "move", Payload: map[string]any{"task_id": "task", "to_step_id": tc.eventStep}}
			for range 2 {
				err := p.OnEvent(context.Background(), event)
				if tc.ambiguous {
					require.ErrorContains(t, err, "ambiguous")
				} else {
					require.NoError(t, err)
				}
			}
			require.Equal(t, tc.wantPuts, puts.Load(), "duplicate deliveries and excluded routes must not produce extra writes")
		})
	}
}

func TestTrackerWritebackRejectsAmbiguousConfiguration(t *testing.T) {
	p, host := newTestPlugin(t)
	host.workflowReader = multiReplyWorkflows{autoStartEntry: true}
	srv := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		fmt.Fprint(out, `{"trackers":[{"id":8,"name":"Custom tracker"}]}`)
	}))
	defer srv.Close()
	client := redmineclient.New(srv.URL, "key", srv.Client())
	route := fieldmapping.TrackerRoute{RedmineTrackerID: 8, WorkflowID: "wf-bug", WorkflowStepID: "diagnosis", AutoStatusWriteback: true, Statuses: []fieldmapping.StatusMapping{{RedmineStatusID: 1, WorkflowStepID: "diagnosis"}, {RedmineStatusID: 3, WorkflowStepID: "diagnosis"}}}
	require.ErrorContains(t, p.validateTrackerRoutes(context.Background(), "ws-1", client, []fieldmapping.TrackerRoute{route}), "only one Redmine status")
	route.AutoStatusWriteback = false
	require.NoError(t, p.validateTrackerRoutes(context.Background(), "ws-1", client, []fieldmapping.TrackerRoute{route}), "legacy inbound-only maps may map several statuses to one step")
	route.AutoStatusWriteback = true
	route.Statuses[1].WorkflowStepID = "first-reply"
	route.AutomaticIntake = &fieldmapping.AutomaticIntakePolicy{FromStatusID: 1, WorkingStatusID: 3}
	require.NoError(t, p.validateTrackerRoutes(context.Background(), "ws-1", client, []fieldmapping.TrackerRoute{route}), "the working status can map to another valid stage without overriding the launch entry")
}
