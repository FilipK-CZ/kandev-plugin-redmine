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

type intakeFixture struct {
	mu                           sync.Mutex
	status, assignee, user, puts int
	failPUT, applyFailedPUT      bool
}

func newIntakeFixture(t *testing.T) (*Service, *fakeHost, Watch, *issues.Service, *intakeFixture) {
	t.Helper()
	host := newFakeHost()
	svc := newWatchService(host)
	w, err := svc.CreateWatch(context.Background(), Watch{WorkspaceID: "ws-1", WorkflowID: "default", ProjectID: 9, MaxInflightTasks: 2, Enabled: true})
	require.NoError(t, err)
	w.TrackerRoutes = []fieldmapping.TrackerRoute{{RedmineTrackerID: 8, WorkflowID: "bugs", WorkflowStepID: "intake", AutomaticIntake: &fieldmapping.AutomaticIntakePolicy{FromStatusID: 1, WorkingStatusID: 3}}}
	w.EntrySteps = map[string][]pluginsdk.WorkflowStep{"bugs": {{ID: "intake", IsStartStep: true, AgentProfileID: "readonly", OnEnterActionTypes: []string{"auto_start_agent"}}}}
	f := &intakeFixture{status: 1, assignee: 7, user: 7}
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

func startedEntry(host *fakeHost, state string) {
	host.sessions = []pluginsdk.Session{{ID: "intake-session", TaskID: "task-1", AgentProfileID: "readonly", ACPSessionID: "acp-session", State: state, StartedAt: "2026-10-26T20:00:00Z"}}
}

func TestAutomaticIntakeClaimWaitsForRealLaunchAndWritesOnlyOnce(t *testing.T) {
	svc, host, w, issuesSvc, f := newIntakeFixture(t)
	ctx := context.Background()
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.Len(t, host.creates, 1)
	require.True(t, host.creates[0].StartAgent)
	require.Equal(t, "readonly", *host.creates[0].Launch.AgentProfileID)
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.Zero(t, f.puts, "best-effort create alone must not claim the issue")
	startedEntry(host, "STARTING")
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.Zero(t, f.puts)
	startedEntry(host, "RUNNING")
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.NoError(t, svc.Poll(ctx, w, issuesSvc))
	require.Equal(t, 1, f.puts)
	require.Len(t, host.creates, 1)
	claim, _, err := host.GetState(ctx, "task", "task-1", intakeClaimKey)
	require.NoError(t, err)
	require.Equal(t, "complete", claim["state"])
	link, _, err := svc.tasklinks.Get(ctx, "task-1")
	require.NoError(t, err)
	require.Equal(t, 3, link.ObservedStatusID)
}

func TestAutomaticIntakePolicyLeavesIneligibleIssuesUnseenUntilAssignment(t *testing.T) {
	svc, host, w, issuesSvc, f := newIntakeFixture(t)
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

func TestAutomaticIntakeClaimNeverWritesAfterEligibilityOrPolicyChanges(t *testing.T) {
	for _, change := range []string{"assignee", "status", "token user", "policy"} {
		t.Run(change, func(t *testing.T) {
			svc, host, w, issuesSvc, f := newIntakeFixture(t)
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			startedEntry(host, "RUNNING")
			switch change {
			case "assignee":
				f.assignee = 99
			case "status":
				f.status = 4
			case "token user":
				f.user = 99
			case "policy":
				w.TrackerRoutes[0].AutomaticIntake = nil
			}
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			require.Zero(t, f.puts)
		})
	}
}

func TestAutomaticIntakeClaimRefusesFailedOrUnrelatedSession(t *testing.T) {
	for _, state := range []string{"FAILED", "CANCELLED", "other-profile"} {
		t.Run(state, func(t *testing.T) {
			svc, host, w, issuesSvc, f := newIntakeFixture(t)
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			startedEntry(host, state)
			if state == "other-profile" {
				host.sessions[0].AgentProfileID, host.sessions[0].State = "implementation", "RUNNING"
			}
			require.Error(t, svc.Poll(context.Background(), w, issuesSvc))
			require.Zero(t, f.puts)
		})
	}
}

func TestAutomaticIntakeUncertainWriteIsReconciledWithoutRepeatingPUT(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			svc, host, w, issuesSvc, f := newIntakeFixture(t)
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			startedEntry(host, "RUNNING")
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
	svc, host, w, issuesSvc, _ := newIntakeFixture(t)
	host.setStateCalls, host.failSetStateAt = 0, 3
	require.ErrorContains(t, svc.Poll(context.Background(), w, issuesSvc), "retained for native inspection")
	require.Len(t, host.tasks, 1)
	require.Empty(t, host.deletedTaskIDs())
	require.ErrorContains(t, svc.Poll(context.Background(), w, issuesSvc), "unconfirmed earlier attempt")
	require.Len(t, host.creates, 1)
	require.ErrorContains(t, svc.DeleteWatch(context.Background(), w.WorkspaceID, w.ID), "unconfirmed auto-start")
}

func TestAutoStartImportUnknownCreateIsNotRepeatedAfterPolicyChange(t *testing.T) {
	svc, host, w, issuesSvc, _ := newIntakeFixture(t)
	host.createErr = status.Error(codes.Unavailable, "response lost")
	require.Error(t, svc.Poll(context.Background(), w, issuesSvc))
	host.createErr = nil
	w.TrackerRoutes[0].AutomaticIntake = nil
	w.EntrySteps["bugs"][0].OnEnterActionTypes = nil
	require.ErrorContains(t, svc.Poll(context.Background(), w, issuesSvc), "unconfirmed earlier attempt")
	require.Len(t, host.creates, 1)
}

func TestKnownRejectedAutoStartCanRetryWithoutAnUncertainLaunch(t *testing.T) {
	svc, host, w, issuesSvc, _ := newIntakeFixture(t)
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
			svc, host, w, issuesSvc, _ := newIntakeFixture(t)
			w.TrackerRoutes[0].AutomaticIntake = nil
			w.EntrySteps["bugs"][0].IsStartStep = tc.startStep
			if !tc.auto {
				w.EntrySteps["bugs"][0].OnEnterActionTypes = nil
			}
			if !tc.routed {
				w.TrackerRoutes = nil
				w.WorkflowID, w.WorkflowStepID = "bugs", "intake"
			}
			require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
			require.Len(t, host.creates, 1)
			require.Equal(t, tc.want, host.creates[0].StartAgent)
		})
	}
}

func TestUnknownAutoStartReservesInflightCapacity(t *testing.T) {
	svc, host, w, issuesSvc, _ := newIntakeFixture(t)
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
			svc, host, first, issuesSvc, _ := newIntakeFixture(t)
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

func TestAutomaticIntakeSupportsAnImplementationEntryAndAttachesItsRepository(t *testing.T) {
	svc, host, w, issuesSvc, f := newIntakeFixture(t)
	w.RepositoryID, w.BaseBranch = "feature-repository", "develop"
	w.TrackerRoutes[0].WorkflowID, w.TrackerRoutes[0].WorkflowStepID = "features", "implementation"
	w.EntrySteps = map[string][]pluginsdk.WorkflowStep{"features": {{ID: "implementation", AgentProfileID: "writer", OnEnterActionTypes: []string{"auto_start_agent"}}}}
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	require.Len(t, host.creates, 1)
	created := host.creates[0]
	require.Equal(t, "features", created.WorkflowID)
	require.Equal(t, "implementation", *created.WorkflowStepID)
	require.Equal(t, "writer", *created.Launch.AgentProfileID)
	require.Len(t, created.Repositories, 1)
	require.Equal(t, w.RepositoryID, created.Repositories[0].RepositoryID)
	require.Equal(t, w.BaseBranch, *created.Repositories[0].BaseBranch)
	metadata := created.Metadata[metadataKeyAutomaticIntake].(map[string]any)
	require.Equal(t, "writer", metadata["profile_id"])
	require.Equal(t, w.RepositoryID, metadata["repository_id"])
	startedEntry(host, "RUNNING")
	host.sessions[0].AgentProfileID = "writer"
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	require.Equal(t, 1, f.puts)
	watches, err := svc.ListWatches(context.Background(), w.WorkspaceID)
	require.NoError(t, err)
	require.Len(t, watches, 1)
	// Runtime bindings above are only local until an operator saves the watch.
	require.NoError(t, svc.UpdateWatch(context.Background(), w))
	watches, err = svc.ListWatches(context.Background(), w.WorkspaceID)
	require.NoError(t, err)
	require.Equal(t, w.RepositoryID, watches[0].RepositoryID)
	require.Equal(t, w.BaseBranch, watches[0].BaseBranch)
}

func TestAutoStartRejectsUnlimitedCapacityAndMissingProfile(t *testing.T) {
	for _, missing := range []string{"limit", "profile"} {
		t.Run(missing, func(t *testing.T) {
			svc, host, w, issuesSvc, _ := newIntakeFixture(t)
			w.TrackerRoutes[0].AutomaticIntake = nil
			if missing == "limit" {
				w.MaxInflightTasks = 0
			} else {
				w.EntrySteps["bugs"][0].AgentProfileID = ""
			}
			require.Error(t, svc.Poll(context.Background(), w, issuesSvc))
			require.Empty(t, host.creates)
		})
	}
}

func TestAutoStartCapsAttemptsEvenWhenEveryNativeCreateIsRejected(t *testing.T) {
	host := newFakeHost()
	host.createErr = status.Error(codes.PermissionDenied, "launch denied")
	svc := newWatchService(host)
	w, err := svc.CreateWatch(context.Background(), Watch{WorkspaceID: "workspace", WorkflowID: "workflow", ProjectID: 1, MaxInflightTasks: 1, Enabled: true})
	require.NoError(t, err)
	w.EntrySteps = map[string][]pluginsdk.WorkflowStep{"workflow": {{ID: "entry", IsStartStep: true, AgentProfileID: "fixed", OnEnterActionTypes: []string{"auto_start_agent"}}}}
	issuesSvc := newIssuesService(t, func(out http.ResponseWriter, _ *http.Request) {
		out.Write([]byte(`{"issues":[{"id":1},{"id":2},{"id":3}],"total_count":3}`))
	})
	require.Error(t, svc.Poll(context.Background(), w, issuesSvc))
	require.Len(t, host.creates, 1, "a failed launch must not free this poll's attempt budget")
}

func TestUnavailableRouteDoesNotBlockAnotherWorkflowsPendingIntake(t *testing.T) {
	svc, host, w, issuesSvc, f := newIntakeFixture(t)
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	startedEntry(host, "RUNNING")
	w.UnavailableWorkflows = map[string]error{"removed-workflow": status.Error(codes.NotFound, "removed")}
	w.TrackerRoutes = append(w.TrackerRoutes, fieldmapping.TrackerRoute{RedmineTrackerID: 99, WorkflowID: "removed-workflow", WorkflowStepID: "entry", AutomaticIntake: &fieldmapping.AutomaticIntakePolicy{FromStatusID: 1, WorkingStatusID: 3}})
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	require.Equal(t, 1, f.puts)
	require.Len(t, host.creates, 1)
}

func TestUnavailableIntakeEntryRecoversWithoutCreatingAnotherTask(t *testing.T) {
	svc, host, w, issuesSvc, f := newIntakeFixture(t)
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	startedEntry(host, "RUNNING")
	w.UnavailableWorkflows = map[string]error{"bugs": status.Error(codes.Unavailable, "temporary failure")}
	require.ErrorContains(t, svc.Poll(context.Background(), w, issuesSvc), "pending intake")
	require.Zero(t, f.puts)
	w.UnavailableWorkflows = nil
	require.NoError(t, svc.Poll(context.Background(), w, issuesSvc))
	require.Equal(t, 1, f.puts)
	require.Len(t, host.creates, 1)
}
