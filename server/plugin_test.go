// Package main tests. Exercises redminePlugin's Plugin/ActionHandler
// behavior against a fake, in-memory Host — no go-plugin subprocess needed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"kandev-plugin-redmine/internal/issues"
	"kandev-plugin-redmine/internal/redmineclient"
	"kandev-plugin-redmine/internal/watch"
)

// fakeHost is an in-memory pluginsdk.Host test double: it actually stores
// state/secrets and captures Tasks().Update calls, so integration tests
// (actions_test.go) can assert on real round trips through the wired
// services, not just that a call didn't panic.
type fakeHost struct {
	pluginsdk.UnimplementedHostData

	mu             sync.Mutex
	state          map[string]map[string]any
	secrets        map[string]string
	updates        []pluginsdk.UpdateTaskInput
	tasks          map[string]*pluginsdk.Task
	nextID         int
	workflowReader pluginsdk.WorkflowReader
}

func newFakeHost() *fakeHost {
	return &fakeHost{state: make(map[string]map[string]any), secrets: make(map[string]string), tasks: make(map[string]*pluginsdk.Task)}
}

func TestConfiguredSyncPollInterval(t *testing.T) {
	t.Setenv("KANDEV_REDMINE_POLL_INTERVAL", "2s")
	require.Equal(t, 2*time.Second, configuredSyncPollInterval())

	t.Setenv("KANDEV_REDMINE_POLL_INTERVAL", "500ms")
	require.Equal(t, defaultSyncPollInterval, configuredSyncPollInterval())

	t.Setenv("KANDEV_REDMINE_POLL_INTERVAL", "not-a-duration")
	require.Equal(t, defaultSyncPollInterval, configuredSyncPollInterval())
}

func TestRunPollLoopPollsImmediatelyAfterStartAndRestart(t *testing.T) {
	// A plugin enable starts a fresh loop after disable stopped the old one.
	// Both loops must poll before the normal production cadence elapses.
	for _, phase := range []string{"start", "restart"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			polls := make(chan struct{}, 1)
			pollNow := make(chan struct{}, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				runPollLoop(ctx, time.Hour, pollNow, func(context.Context) { polls <- struct{}{} })
			}()

			select {
			case <-polls:
			case <-time.After(time.Second):
				t.Fatal("initial poll did not run immediately")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("poll loop did not stop after cancellation")
			}
		})
	}
}

func TestRunPollLoopWakesPromptlyAfterSuccessfulRedmineMutation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polls := make(chan struct{}, 2)
	pollNow := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPollLoop(ctx, time.Hour, pollNow, func(context.Context) { polls <- struct{}{} })
	}()
	<-polls // initial startup poll
	pollNow <- struct{}{}
	select {
	case <-polls:
	case <-time.After(time.Second):
		t.Fatal("mutation-triggered poll did not run promptly")
	}
	cancel()
	<-done
}

func stateKeyOf(scope, scopeID, key string) string { return scope + "/" + scopeID + "/" + key }

func (h *fakeHost) GetState(_ context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.state[stateKeyOf(scope, scopeID, key)]
	return v, ok, nil
}

func (h *fakeHost) SetState(_ context.Context, scope, scopeID, key string, value map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	h.state[stateKeyOf(scope, scopeID, key)] = out
	return nil
}

func (h *fakeHost) DeleteState(_ context.Context, scope, scopeID, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, stateKeyOf(scope, scopeID, key))
	return nil
}

func (h *fakeHost) ListState(context.Context, string, string) ([]pluginsdk.StateEntry, error) {
	return nil, nil
}
func (h *fakeHost) GetConfig(context.Context) (map[string]any, error) { return map[string]any{}, nil }
func (h *fakeHost) RevealSecret(context.Context, string) (string, error) {
	return "", nil
}

func (h *fakeHost) GetSecret(_ context.Context, key string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.secrets[key]
	return v, ok, nil
}
func (h *fakeHost) SetSecret(_ context.Context, key, value string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.secrets[key] = value
	return nil
}
func (h *fakeHost) DeleteSecret(_ context.Context, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.secrets, key)
	return nil
}
func (h *fakeHost) EmitEvent(context.Context, string, map[string]any) error { return nil }

func (h *fakeHost) Tasks() pluginsdk.TaskReader {
	return fakeTaskReader{TaskReader: pluginsdk.UnimplementedHostData{}.Tasks(), host: h}
}

func (h *fakeHost) Workflows() pluginsdk.WorkflowReader {
	if h.workflowReader != nil {
		return h.workflowReader
	}
	return fakeWorkflowReader{}
}

func (h *fakeHost) PluginOwnedTaskTrees() pluginsdk.PluginOwnedTaskTreeManager {
	return fakeTaskTreeManager{host: h}
}

// fakeWorkflowReader returns one fixed workflow with two steps, enough to
// exercise the workflows.list action without a real Kandev host.
type fakeWorkflowReader struct{}

func (fakeWorkflowReader) List(context.Context, string, pluginsdk.Page) ([]pluginsdk.Workflow, *pluginsdk.PageInfo, error) {
	return []pluginsdk.Workflow{{ID: "wf-1", Name: "Default"}}, nil, nil
}

func (fakeWorkflowReader) ListSteps(context.Context, string) ([]pluginsdk.WorkflowStep, error) {
	return []pluginsdk.WorkflowStep{
		{ID: "step-backlog", Name: "Backlog"},
		{ID: "step-done", Name: "Done"},
	}, nil
}

func (h *fakeHost) updateCalls() []pluginsdk.UpdateTaskInput {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pluginsdk.UpdateTaskInput(nil), h.updates...)
}

type fakeTaskReader struct {
	pluginsdk.TaskReader
	host *fakeHost
}

func (r fakeTaskReader) Update(_ context.Context, in pluginsdk.UpdateTaskInput) (*pluginsdk.Task, error) {
	r.host.mu.Lock()
	r.host.updates = append(r.host.updates, in)
	r.host.mu.Unlock()
	return &pluginsdk.Task{ID: in.ID}, nil
}

func (r fakeTaskReader) Create(_ context.Context, in pluginsdk.CreateTaskInput) (*pluginsdk.Task, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	r.host.nextID++
	task := &pluginsdk.Task{ID: fmt.Sprintf("task-%d", r.host.nextID), WorkspaceID: in.WorkspaceID, Title: in.Title, Description: in.Description, State: "RUNNING", Metadata: in.Metadata}
	task.WorkflowID = in.WorkflowID
	if in.WorkflowStepID != nil {
		task.WorkflowStepID = *in.WorkflowStepID
	}
	r.host.tasks[task.ID] = task
	return task, nil
}

func (r fakeTaskReader) Get(_ context.Context, taskID string) (*pluginsdk.Task, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	task, found := r.host.tasks[taskID]
	if !found {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	copyTask := *task
	return &copyTask, nil
}

type fakeTaskTreeManager struct{ host *fakeHost }

func (m fakeTaskTreeManager) Preview(_ context.Context, taskID string) ([]pluginsdk.Task, error) {
	task, err := fakeTaskReader{host: m.host}.Get(context.Background(), taskID)
	if err != nil {
		return nil, nil
	}
	return []pluginsdk.Task{*task}, nil
}

func (m fakeTaskTreeManager) Delete(_ context.Context, taskID string) ([]string, error) {
	m.host.mu.Lock()
	defer m.host.mu.Unlock()
	if _, found := m.host.tasks[taskID]; !found {
		return nil, nil
	}
	delete(m.host.tasks, taskID)
	return []string{taskID}, nil
}

var _ pluginsdk.Host = (*fakeHost)(nil)
var _ pluginsdk.PluginOwnedTaskTreeHost = (*fakeHost)(nil)

func TestRedminePlugin_HostRoundTrip(t *testing.T) {
	p := &redminePlugin{}
	require.Nil(t, p.Host())

	host := newFakeHost()
	p.SetHost(host)
	t.Cleanup(p.stop)
	require.Same(t, pluginsdk.Host(host), p.Host())
}

func TestRedminePlugin_OnEvent_IgnoresNonTaskMovedEvents(t *testing.T) {
	p := &redminePlugin{}
	p.SetHost(newFakeHost())
	t.Cleanup(p.stop)
	err := p.OnEvent(context.Background(), &pluginsdk.Event{EventID: "e1", EventType: "task.created"})
	require.NoError(t, err)
}

func TestRedminePlugin_OnEvent_BeforeHostInjected_IsNoOp(t *testing.T) {
	p := &redminePlugin{}
	err := p.OnEvent(context.Background(), &pluginsdk.Event{EventID: "e1", EventType: "task.moved", Payload: map[string]any{"task_id": "t1", "to_step_id": "step-done"}})
	require.NoError(t, err)
}

func TestRedminePlugin_HandleWebhook_UnimplementedWithoutOverride(t *testing.T) {
	p := &redminePlugin{}
	resp, err := p.HandleWebhook(context.Background(), &pluginsdk.WebhookRequest{WebhookKey: "ping", Method: "POST"})
	require.NoError(t, err)
	require.Equal(t, int32(404), resp.Status)
}

func TestRedminePlugin_StopTerminatesOwnedLoops(t *testing.T) {
	p := &redminePlugin{}
	p.SetHost(newFakeHost())
	p.stop()

	p.mu.Lock()
	ready := p.ready
	done := p.stopDone
	p.mu.Unlock()
	require.False(t, ready)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("plugin stop did not wait for owned loops")
	}
}

func TestRedminePlugin_StopIsConcurrentAndIdempotent(t *testing.T) {
	p := &redminePlugin{}
	p.SetHost(newFakeHost())
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.stop()
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("concurrent stop calls deadlocked")
	}
	// A later stop observes the closed stop channel and is a no-op.
	p.stop()
}

func TestRedminePlugin_PollWatchesLinksCreatedTaskThroughSharedService(t *testing.T) {
	p, _ := newTestPlugin(t)
	w, err := p.watchSvc.CreateWatch(context.Background(), watch.Watch{WorkspaceID: "ws-1", ProjectID: 1, Enabled: true})
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(res http.ResponseWriter, _ *http.Request) {
		_, _ = res.Write([]byte(`{"issues":[{"id":42,"subject":"from watcher"}],"total_count":1}`))
	}))
	defer srv.Close()
	issuesSvc := issues.New(redmineclient.New(srv.URL, "key", srv.Client()))

	require.NoError(t, p.pollWatches(context.Background(), "ws-1", issuesSvc))
	taskID, found, err := p.tasklinkSvc.TaskIDForIssue(context.Background(), "ws-1", 42)
	require.NoError(t, err)
	require.True(t, found)
	link, found, err := p.tasklinkSvc.Get(context.Background(), taskID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 42, link.IssueID)
	require.Equal(t, w.WorkspaceID, link.WorkspaceID)
}
