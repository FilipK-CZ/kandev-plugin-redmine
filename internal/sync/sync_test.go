package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"kandev-plugin-redmine/internal/fieldmapping"
	"kandev-plugin-redmine/internal/issues"
	"kandev-plugin-redmine/internal/redmineclient"
	"kandev-plugin-redmine/internal/tasklink"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testMapping() fieldmapping.Mapping {
	return fieldmapping.Mapping{
		Statuses: []fieldmapping.StatusMapping{
			{RedmineStatusID: 1, RedmineName: "Triage", IsClosed: false, WorkflowStepID: "step-backlog"},
			{RedmineStatusID: 2, RedmineName: "Shipped", IsClosed: true, WorkflowStepID: "step-done"},
		},
		Priorities: []fieldmapping.PriorityMapping{{RedminePriorityID: 5, TaskPriority: "high"}},
	}
}

func TestPollInbound_LinkedIssueStatusChange_MovesTaskToMappedStep(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "*", r.URL.Query().Get("status_id"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"subject":"s","status":{"id":2,"name":"Shipped"},"updated_on":"2026-01-01T00:00:00Z"}],"total_count":1}`))
	}))
	defer srv.Close()

	client := redmineclient.New(srv.URL, "key", srv.Client())
	issuesSvc := issues.New(client)

	err := svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{})
	require.NoError(t, err)

	calls := host.moveCalls()
	require.Len(t, calls, 1)
	require.Equal(t, "task-1", calls[0].TaskID)
	require.Equal(t, "step-done", calls[0].WorkflowStepID)
	require.Empty(t, host.updateCalls(), "workflow step changes must not use Update")
	cursor, found, err := host.GetState(context.Background(), stateScope, "ws-1", cursorKey)
	require.NoError(t, err)
	require.True(t, found, "a successful Move must still advance the inbound cursor")
	require.Equal(t, "2026-01-01T00:00:00Z", cursor["updated_on"])
}

func TestPollInbound_UnlinkedIssue_DoesNotTouchAnyTask(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"subject":"s","status":{"id":2}}],"total_count":1}`))
	}))
	defer srv.Close()

	client := redmineclient.New(srv.URL, "key", srv.Client())
	issuesSvc := issues.New(client)

	err := svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{})
	require.NoError(t, err)
	require.Empty(t, host.updateCalls())
	require.Empty(t, host.moveCalls())
}

func TestPollInbound_NotFoundTaskSelfHealsStaleLink(t *testing.T) {
	host := newFakeHost()
	host.moveErr = status.Error(codes.NotFound, "task deleted")
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"subject":"s","status":{"id":2},"updated_on":"2026-01-01T00:00:00Z"}],"total_count":1}`))
	}))
	defer srv.Close()
	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issues.New(redmineclient.New(srv.URL, "key", srv.Client())), testMapping(), []int{1}, Options{}))
	_, found, err := tl.Get(context.Background(), "task-1")
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = tl.TaskIDForIssue(context.Background(), "ws-1", 42)
	require.NoError(t, err)
	require.False(t, found)
}

func TestPollInbound_TitleDescriptionSync_OnlyWhenEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"subject":"New subject","description":"New body","status":{"id":1}}],"total_count":1}`))
	}))
	defer srv.Close()
	client := redmineclient.New(srv.URL, "key", srv.Client())
	issuesSvc := issues.New(client)

	t.Run("disabled: no title/description update", func(t *testing.T) {
		host := newFakeHost()
		tl := tasklink.New(host)
		svc := New(host, tl)
		require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))

		require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{SyncTitleDescription: false}))
		require.Empty(t, host.updateCalls())
		moves := host.moveCalls()
		require.Len(t, moves, 1) // status still applies (mapped to step-backlog)
		require.Equal(t, "step-backlog", moves[0].WorkflowStepID)
	})

	t.Run("enabled: title/description update", func(t *testing.T) {
		host := newFakeHost()
		tl := tasklink.New(host)
		svc := New(host, tl)
		require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))

		require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{SyncTitleDescription: true}))
		calls := host.updateCalls()
		require.Len(t, calls, 1)
		require.NotNil(t, calls[0].Title)
		require.Equal(t, "New subject", *calls[0].Title)
		require.NotNil(t, calls[0].Description)
		require.Equal(t, "New body", *calls[0].Description)
		require.Nil(t, calls[0].WorkflowStepID, "fake Host rejects step changes on Update")
		moves := host.moveCalls()
		require.Len(t, moves, 1)
		require.Equal(t, "step-backlog", moves[0].WorkflowStepID)
	})
}

func TestPollInbound_OverlappedUnchangedTitleDescriptionWritesOnce(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"subject":"Stable title","description":"Stable description","status":{"id":99},"updated_on":"2026-01-01T00:00:00Z"}],"total_count":1}`))
	}))
	defer srv.Close()
	issuesSvc := issues.New(redmineclient.New(srv.URL, "key", srv.Client()))
	// Status 99 is deliberately unmapped, isolating title/description
	// idempotency from workflow-step reconciliation.
	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{SyncTitleDescription: true}))
	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{SyncTitleDescription: true}))
	require.Len(t, host.updateCalls(), 1)
}

func TestPollInbound_OverlappedMappedStatusDoesNotMoveAlreadyAlignedTask(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"status":{"id":2},"updated_on":"2026-01-01T00:00:00Z"}],"total_count":1}`))
	}))
	defer srv.Close()
	issuesSvc := issues.New(redmineclient.New(srv.URL, "key", srv.Client()))
	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{}))
	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{}))
	require.Empty(t, host.updateCalls())
	require.Len(t, host.moveCalls(), 1)
	for _, call := range host.moveCalls() {
		require.Equal(t, "step-done", call.WorkflowStepID)
	}
}

func TestPollInbound_ManualMoveAwayIsRestoredFromMappedRedmineStatus(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"status":{"id":2},"updated_on":"2026-01-01T00:00:00Z"}],"total_count":1}`))
	}))
	defer srv.Close()
	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issues.New(redmineclient.New(srv.URL, "key", srv.Client())), testMapping(), []int{1}, Options{}))
	require.Empty(t, host.updateCalls())
	require.Len(t, host.moveCalls(), 1)
}

func TestPollInbound_AppliesPriorityWithoutChangingWorkflowStep(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"status":{"id":99},"tracker":{"id":3},"priority":{"id":5},"updated_on":"2026-01-01T00:00:00Z"}],"total_count":1}`))
	}))
	defer srv.Close()
	issuesSvc := issues.New(redmineclient.New(srv.URL, "key", srv.Client()))

	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{}))
	require.Len(t, host.updateCalls(), 1)
	require.NotNil(t, host.updateCalls()[0].Priority)
	require.Equal(t, "high", *host.updateCalls()[0].Priority)
	require.Equal(t, "high", host.task.Priority)
	require.Empty(t, host.moveCalls())

	// The overlap poll is idempotent once the task priority agrees with Redmine.
	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{}))
	require.Len(t, host.updateCalls(), 1)
}

func TestPollInbound_TrackerDifferenceAloneDoesNotUpdateTask(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"status":{"id":99},"tracker":{"id":4}}],"total_count":1}`))
	}))
	defer srv.Close()

	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issues.New(redmineclient.New(srv.URL, "key", srv.Client())), testMapping(), []int{1}, Options{}))
	require.Empty(t, host.updateCalls())
}

func TestPollInbound_CursorAdvancesAndPersistsAcrossRestarts(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)

	var sawSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSince = r.URL.Query().Get("updated_on")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"issues":[{"id":42,"subject":"s","status":{"id":1},"updated_on":"2026-01-01T12:00:00Z"}],"total_count":1}`))
	}))
	defer srv.Close()
	client := redmineclient.New(srv.URL, "key", srv.Client())
	issuesSvc := issues.New(client)

	svc1 := New(host, tl)
	require.NoError(t, svc1.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{}))
	require.Empty(t, sawSince) // first poll: no cursor yet

	// "Restart" — a fresh Service backed by the same host must resume from
	// the persisted cursor, not from zero.
	svc2 := New(host, tl)
	require.NoError(t, svc2.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{}))
	require.Contains(t, sawSince, ">=2026-01-01T11:59:59Z") // cursor minus 1s overlap
}

func TestPollInbound_NoProjectsSelected_IsNoOp(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	client := redmineclient.New(srv.URL, "key", srv.Client())
	issuesSvc := issues.New(client)

	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), nil, Options{}))
	require.False(t, called)
}

func TestPushWriteback_MappedStepChange_IssuesPUTAndRecordsEchoSuppression(t *testing.T) {
	host := newFakeHost()
	host.task.WorkflowStepID = "step-done"
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))

	var putStatusID any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"issue":{"id":42,"status":{"id":1}}}`))
			return
		}
		require.Equal(t, http.MethodPut, r.Method)
		var body map[string]any
		_ = readJSONBody(r, &body)
		issueBody := body["issue"].(map[string]any)
		putStatusID = issueBody["status_id"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := redmineclient.New(srv.URL, "key", srv.Client())
	issuesSvc := issues.New(client)

	err := svc.PushWriteback(context.Background(), "task-1", "step-done", testMapping(), issuesSvc, Options{AutoStatusWriteback: true})
	require.NoError(t, err)
	require.EqualValues(t, 2, putStatusID)

	link, found, err := tl.Get(context.Background(), "task-1")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, link.LastPushedStatusID)
	require.Equal(t, 2, *link.LastPushedStatusID)
}

func TestPushWriteback_Disabled_IssuesNoPUT(t *testing.T) {
	host := newFakeHost()
	host.task.WorkflowStepID = "step-done"
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	client := redmineclient.New(srv.URL, "key", srv.Client())
	issuesSvc := issues.New(client)

	err := svc.PushWriteback(context.Background(), "task-1", "step-done", testMapping(), issuesSvc, Options{AutoStatusWriteback: false})
	require.NoError(t, err)
	require.False(t, called)
}

func TestPushWriteback_AlreadyPushedSameStatus_IsIdempotent(t *testing.T) {
	host := newFakeHost()
	host.task.WorkflowStepID = "step-done"
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	require.NoError(t, tl.RecordPushedStatus(context.Background(), "task-1", 2)) // already pushed

	putCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"issue":{"id":42,"status":{"id":2}}}`))
			return
		}
		putCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := redmineclient.New(srv.URL, "key", srv.Client())
	issuesSvc := issues.New(client)

	err := svc.PushWriteback(context.Background(), "task-1", "step-done", testMapping(), issuesSvc, Options{AutoStatusWriteback: true})
	require.NoError(t, err)
	require.Equal(t, 0, putCount, "a duplicate write-back for the same live Redmine status must not issue a redundant PUT")
}

func TestPushWriteback_StaleMarkerDoesNotSuppressRemoteDrift(t *testing.T) {
	host := newFakeHost()
	host.task.WorkflowStepID = "step-done"
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	require.NoError(t, tl.RecordPushedStatus(context.Background(), "task-1", 2))
	putCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"issue":{"id":42,"status":{"id":1}}}`))
			return
		}
		putCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	require.NoError(t, svc.PushWriteback(context.Background(), "task-1", "step-done", testMapping(), issues.New(redmineclient.New(srv.URL, "key", srv.Client())), Options{AutoStatusWriteback: true}))
	require.Equal(t, 1, putCount)
}

func TestEchoSuppression_WriteBackThenInboundPoll_DoesNotBounceTask(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"issues":[{"id":42,"subject":"s","status":{"id":2},"updated_on":"2026-01-01T00:00:00Z"}],"total_count":1}`))
		}
	}))
	defer srv.Close()
	client := redmineclient.New(srv.URL, "key", srv.Client())
	issuesSvc := issues.New(client)

	require.NoError(t, svc.PushWriteback(context.Background(), "task-1", "step-done", testMapping(), issuesSvc, Options{AutoStatusWriteback: true}))
	require.Len(t, host.updateCalls(), 0, "PushWriteback itself never calls Tasks().Update")

	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{}))
	require.Empty(t, host.updateCalls(), "the inbound poll observing our own pushed status must not re-apply/bounce the task")
}

func TestPushWriteback_DuplicateAfterEchoConsumptionReadsLiveStatusAndSkipsPUT(t *testing.T) {
	host := newFakeHost()
	host.task.WorkflowStepID = "step-done"
	tl := tasklink.New(host)
	svc := New(host, tl)
	require.NoError(t, tl.Set(context.Background(), "task-1", "ws-1", 42, "url"))
	putCount := 0
	redmineStatus := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path == "/issues/42.json" {
				_, _ = w.Write([]byte(fmt.Sprintf(`{"issue":{"id":42,"status":{"id":%d}}}`, redmineStatus)))
				return
			}
			_, _ = w.Write([]byte(`{"issues":[{"id":42,"subject":"s","status":{"id":2},"updated_on":"2026-01-01T00:00:00Z"}],"total_count":1}`))
		case http.MethodPut:
			putCount++
			redmineStatus = 2
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	issuesSvc := issues.New(redmineclient.New(srv.URL, "key", srv.Client()))

	// First event writes, inbound consumes its one-shot marker, then a
	// duplicate event observes Redmine already has the desired status.
	require.NoError(t, svc.PushWriteback(context.Background(), "task-1", "step-done", testMapping(), issuesSvc, Options{AutoStatusWriteback: true}))
	require.Equal(t, 1, putCount)
	require.NoError(t, svc.PollInbound(context.Background(), "ws-1", issuesSvc, testMapping(), []int{1}, Options{}))
	require.NoError(t, svc.PushWriteback(context.Background(), "task-1", "step-done", testMapping(), issuesSvc, Options{AutoStatusWriteback: true}))
	require.Equal(t, 1, putCount)
}

func TestSaveAndGetOptions_RoundTrip(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)

	require.NoError(t, svc.SaveOptions(context.Background(), "ws-1", Options{AutoStatusWriteback: true, SyncTitleDescription: false}))
	got, err := svc.GetOptions(context.Background(), "ws-1")
	require.NoError(t, err)
	require.True(t, got.AutoStatusWriteback)
	require.False(t, got.SyncTitleDescription)
}

func TestGetOptions_NoneSaved_DefaultsToManualOnly(t *testing.T) {
	host := newFakeHost()
	tl := tasklink.New(host)
	svc := New(host, tl)

	got, err := svc.GetOptions(context.Background(), "ws-1")
	require.NoError(t, err)
	require.False(t, got.AutoStatusWriteback)
	require.False(t, got.SyncTitleDescription)
}

func readJSONBody(r *http.Request, out *map[string]any) error {
	return json.NewDecoder(r.Body).Decode(out)
}
