package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"kandev-plugin-redmine/internal/fieldmapping"
	"kandev-plugin-redmine/internal/issues"
)

type diagnosticFixture struct {
	mu                           sync.Mutex
	status, assignee, user, puts int
	failPUT, applyFailedPUT      bool
}

func newDiagnosticFixture(t *testing.T) (*Service, *fakeHost, Watch, *issues.Service, *diagnosticFixture) {
	t.Helper()
	host := newFakeHost()
	svc := newWatchService(host)
	w, err := svc.CreateWatch(context.Background(), Watch{WorkspaceID: "ws-1", WorkflowID: "default", ProjectID: 9, Enabled: true})
	require.NoError(t, err)
	w.TrackerRoutes = []fieldmapping.TrackerRoute{{RedmineTrackerID: 8, WorkflowID: "bugs", WorkflowStepID: "diagnosis", Diagnosis: &fieldmapping.DiagnosticPolicy{FromStatusID: 1, WorkingStatusID: 3}}}
	w.EntrySteps = map[string][]pluginsdk.WorkflowStep{"bugs": {{ID: "diagnosis", IsStartStep: true, AgentProfileID: "readonly", OnEnterActionTypes: []string{"auto_start_agent"}}}}
	f := &diagnosticFixture{status: 1, assignee: 7, user: 7}
	issuesSvc := newIssuesService(t, func(out http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/users/current.json":
			fmt.Fprintf(out, `{"user":{"id":%d}}`, f.user)
		case r.Method == http.MethodPut:
			var body map[string]map[string]int
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, map[string]map[string]int{"issue": {"status_id": 3}}, body, "claim changes status only, never assignment or customer notes")
			f.puts++
			if !f.failPUT || f.applyFailedPUT {
				f.status = 3
			}
			if f.failPUT {
				out.WriteHeader(http.StatusServiceUnavailable)
			}
		default:
			issue := fmt.Sprintf(`{"id":42,"project":{"id":9},"tracker":{"id":8},"status":{"id":%d},"assigned_to":{"id":%d},"subject":"Broken label"}`, f.status, f.assignee)
			if r.URL.Path == "/issues.json" {
				fmt.Fprintf(out, `{"issues":[%s],"total_count":1}`, issue)
			} else {
				fmt.Fprintf(out, `{"issue":%s}`, issue)
			}
		}
	})
	return svc, host, w, issuesSvc, f
}

func startedDiagnosis(host *fakeHost, state string) {
	host.sessions = []pluginsdk.Session{{ID: "diagnostic-session", TaskID: "task-1", AgentProfileID: "readonly", ACPSessionID: "acp-session", State: state, StartedAt: "2026-10-26T20:00:00Z"}}
}

func TestDiagnosticClaimWaitsForRealLaunchAndWritesOnlyOnce(t *testing.T) {
	svc, host, w, issuesSvc, f := newDiagnosticFixture(t)
	ctx := context.Background()
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.Len(t, host.creates, 1)
	require.True(t, host.creates[0].StartAgent)
	require.Equal(t, "readonly", *host.creates[0].Launch.AgentProfileID)
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.Zero(t, f.puts, "best-effort create alone must not claim the issue")
	startedDiagnosis(host, "STARTING")
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.Zero(t, f.puts)
	startedDiagnosis(host, "RUNNING")
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.Equal(t, 1, f.puts)
	require.Len(t, host.creates, 1)
	claim, _, err := host.GetState(ctx, "task", "task-1", diagnosticClaimKey)
	require.NoError(t, err)
	require.Equal(t, "complete", claim["state"])
	link, _, err := svc.tasklinks.Get(ctx, "task-1")
	require.NoError(t, err)
	require.Equal(t, 3, link.ObservedStatusID)
}

func TestDiagnosticPolicyLeavesIneligibleIssuesUnseenUntilAssignment(t *testing.T) {
	svc, host, w, issuesSvc, f := newDiagnosticFixture(t)
	f.assignee = 99
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	require.Empty(t, host.creates)
	f.assignee, f.status = 7, 3
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	require.Empty(t, host.creates)
	f.status = 1
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	require.Len(t, host.creates, 1)
}

func TestDiagnosticClaimNeverWritesAfterEligibilityOrPolicyChanges(t *testing.T) {
	for _, change := range []string{"assignee", "status", "token user", "policy"} {
		t.Run(change, func(t *testing.T) {
			svc, host, w, issuesSvc, f := newDiagnosticFixture(t)
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			startedDiagnosis(host, "RUNNING")
			switch change {
			case "assignee":
				f.assignee = 99
			case "status":
				f.status = 4
			case "token user":
				f.user = 99
			case "policy":
				w.TrackerRoutes[0].Diagnosis = nil
			}
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			require.Zero(t, f.puts)
		})
	}
}

func TestDiagnosticClaimRefusesFailedOrUnrelatedSession(t *testing.T) {
	for _, state := range []string{"FAILED", "CANCELLED", "other-profile"} {
		t.Run(state, func(t *testing.T) {
			svc, host, w, issuesSvc, f := newDiagnosticFixture(t)
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			startedDiagnosis(host, state)
			if state == "other-profile" {
				host.sessions[0].AgentProfileID, host.sessions[0].State = "implementation", "RUNNING"
			}
			require.Error(t, svc.Poll(context.Background(), w, issuesSvc))
			require.Zero(t, f.puts)
		})
	}
}

func TestDiagnosticUncertainWriteIsReconciledWithoutRepeatingPUT(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			svc, host, w, issuesSvc, f := newDiagnosticFixture(t)
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			startedDiagnosis(host, "RUNNING")
			f.failPUT, f.applyFailedPUT = true, applied
			require.ErrorContains(t, svc.Poll(context.Background(), w, issuesSvc), "no automatic retry")
			err := svc.Poll(context.Background(), w, issuesSvc)
			if applied {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "requires inspection")
			}
			require.Equal(t, 1, f.puts)
		})
	}
}

func TestAutoStartImportRetainsRunningTaskWhenLinkPersistenceFails(t *testing.T) {
	svc, host, w, issuesSvc, _ := newDiagnosticFixture(t)
	host.setStateCalls, host.failSetStateAt = 0, 3
	require.ErrorContains(t, svc.Poll(context.Background(), w, issuesSvc), "retained for native inspection")
	require.Len(t, host.tasks, 1)
	require.Empty(t, host.deletedTaskIDs())
	require.ErrorContains(t, svc.Poll(context.Background(), w, issuesSvc), "unconfirmed earlier attempt")
	require.Len(t, host.creates, 1)
	require.ErrorContains(t, svc.DeleteWatch(context.Background(), w.WorkspaceID, w.ID), "unconfirmed auto-start")
}

func TestAutoStartImportUnknownCreateIsNotRepeatedAfterPolicyChange(t *testing.T) {
	svc, host, w, issuesSvc, _ := newDiagnosticFixture(t)
	host.createErr = status.Error(codes.Unavailable, "response lost")
	require.Error(t, svc.Poll(context.Background(), w, issuesSvc))
	host.createErr = nil
	w.TrackerRoutes[0].Diagnosis = nil
	w.EntrySteps["bugs"][0].OnEnterActionTypes = nil
	require.ErrorContains(t, svc.Poll(context.Background(), w, issuesSvc), "unconfirmed earlier attempt")
	require.Len(t, host.creates, 1)
}

func TestKnownRejectedAutoStartCanRetryWithoutAnUncertainLaunch(t *testing.T) {
	svc, host, w, issuesSvc, _ := newDiagnosticFixture(t)
	host.createErr = status.Error(codes.PermissionDenied, "tasks grant required")
	require.Error(t, svc.Poll(context.Background(), w, issuesSvc))
	host.createErr = nil
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	require.Len(t, host.tasks, 1)
	require.Len(t, host.creates, 2)
}

func TestNativeAutoStartIsExplicitAndDoesNotEnableOldIntermediateImports(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		routed, startStep, auto, want bool
	}{
		{"routed entry", true, true, true, true},
		{"manual route", true, true, false, false},
		{"native default entry", false, true, true, true},
		{"old intermediate filter", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, host, w, issuesSvc, _ := newDiagnosticFixture(t)
			w.TrackerRoutes[0].Diagnosis = nil
			w.EntrySteps["bugs"][0].IsStartStep = tc.startStep
			if !tc.auto {
				w.EntrySteps["bugs"][0].OnEnterActionTypes = nil
			}
			if !tc.routed {
				w.TrackerRoutes = nil
				w.WorkflowID, w.WorkflowStepID = "bugs", "diagnosis"
			}
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			require.Len(t, host.creates, 1)
			require.Equal(t, tc.want, host.creates[0].StartAgent)
		})
	}
}

func TestUnknownAutoStartReservesInflightCapacity(t *testing.T) {
	svc, host, w, issuesSvc, _ := newDiagnosticFixture(t)
	w.MaxInflightTasks = 1
	host.createErr = status.Error(codes.Unavailable, "result lost")
	require.Error(t, svc.Poll(context.Background(), w, issuesSvc))
	host.createErr = nil
	require.ErrorContains(t, svc.Poll(context.Background(), w, issuesSvc), "unconfirmed earlier attempt")
	require.Len(t, host.creates, 1, "an unindexed possibly-running task still occupies the only slot")
}

func TestOverlappingWatchesCannotLaunchAnAlreadyLinkedOrUncertainIssueTwice(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			svc, host, first, issuesSvc, _ := newDiagnosticFixture(t)
			second, err := svc.CreateWatch(context.Background(), Watch{WorkspaceID: first.WorkspaceID, WorkflowID: first.WorkflowID, ProjectID: first.ProjectID, Enabled: true})
			require.NoError(t, err)
			second.TrackerRoutes, second.EntrySteps = first.TrackerRoutes, first.EntrySteps
			if uncertain {
				host.createErr = status.Error(codes.Unavailable, "lost reply")
			}
			err = svc.Poll(context.Background(), first, issuesSvc)
			if uncertain {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			host.createErr = nil
			err = svc.Poll(context.Background(), second, issuesSvc)
			if uncertain {
				require.ErrorContains(t, err, "unconfirmed earlier attempt")
			} else {
				require.NoError(t, err)
			}
			require.Len(t, host.creates, 1)
		})
	}
}
