package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"kandev-plugin-redmine/internal/tasklink"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeHost is an in-memory pluginsdk.Host test double with a capturing,
// stateful Tasks() (Create/Get) and PluginOwnedTaskTrees, so tests can
// assert on real create/throttle/cascade-delete round trips.
type fakeHost struct {
	pluginsdk.UnimplementedHostData

	mu             sync.Mutex
	state          map[string]map[string]any
	tasks          map[string]*pluginsdk.Task
	nextID         int
	deleted        []string
	creates        []pluginsdk.CreateTaskInput
	getStateErr    error
	setStateErr    error
	setStateCalls  int
	failSetStateAt int
	getTaskErr     error
	createErr      error
	sessions       []pluginsdk.Session
}

func newFakeHost() *fakeHost {
	return &fakeHost{state: make(map[string]map[string]any), tasks: make(map[string]*pluginsdk.Task)}
}

func newWatchService(host *fakeHost) *Service {
	return New(host, tasklink.New(host))
}

// hostWithoutTaskTrees forwards the regular Host contract but deliberately
// does not expose PluginOwnedTaskTrees, for fail-closed cleanup tests.
type hostWithoutTaskTrees struct{ pluginsdk.Host }

func key(scope, scopeID, k string) string { return scope + "/" + scopeID + "/" + k }

func (h *fakeHost) GetState(_ context.Context, scope, scopeID, k string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.getStateErr != nil {
		return nil, false, h.getStateErr
	}
	v, ok := h.state[key(scope, scopeID, k)]
	return v, ok, nil
}

func (h *fakeHost) SetState(_ context.Context, scope, scopeID, k string, value map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setStateCalls++
	if h.failSetStateAt > 0 && h.setStateCalls == h.failSetStateAt {
		return fmt.Errorf("injected state failure")
	}
	if h.setStateErr != nil {
		err := h.setStateErr
		h.setStateErr = nil
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	h.state[key(scope, scopeID, k)] = out
	return nil
}

func (h *fakeHost) DeleteState(_ context.Context, scope, scopeID, k string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, key(scope, scopeID, k))
	return nil
}

func (h *fakeHost) ListState(_ context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var entries []pluginsdk.StateEntry
	for k, value := range h.state {
		if tail, found := strings.CutPrefix(k, key(scope, scopeID, "")); found {
			entries = append(entries, pluginsdk.StateEntry{Key: tail, Value: value})
		}
	}
	return entries, nil
}
func (h *fakeHost) GetConfig(context.Context) (map[string]any, error) { return map[string]any{}, nil }
func (h *fakeHost) RevealSecret(context.Context, string) (string, error) {
	return "", nil
}
func (h *fakeHost) GetSecret(context.Context, string) (string, bool, error) { return "", false, nil }
func (h *fakeHost) SetSecret(context.Context, string, string) error         { return nil }
func (h *fakeHost) DeleteSecret(context.Context, string) error              { return nil }
func (h *fakeHost) EmitEvent(context.Context, string, map[string]any) error { return nil }

func (h *fakeHost) Tasks() pluginsdk.TaskReader {
	return fakeTaskReader{TaskReader: pluginsdk.UnimplementedHostData{}.Tasks(), host: h}
}

func (h *fakeHost) PluginOwnedTaskTrees() pluginsdk.PluginOwnedTaskTreeManager {
	return fakeTaskTreeManager{host: h}
}

// setTaskState lets a test move a created task to a terminal state, to
// exercise the throttle's "still open" check.
func (h *fakeHost) setTaskState(taskID, state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.tasks[taskID]; ok {
		t.State = state
	}
}

func (h *fakeHost) deletedTaskIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.deleted...)
}

type fakeTaskReader struct {
	pluginsdk.TaskReader
	host *fakeHost
}

func (r fakeTaskReader) Create(_ context.Context, in pluginsdk.CreateTaskInput) (*pluginsdk.Task, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	r.host.nextID++
	r.host.creates = append(r.host.creates, in)
	if r.host.createErr != nil {
		return nil, r.host.createErr
	}
	task := &pluginsdk.Task{
		ID: fmt.Sprintf("task-%d", r.host.nextID), WorkspaceID: in.WorkspaceID,
		WorkflowID: in.WorkflowID,
		Title:      in.Title, Description: in.Description, State: "RUNNING",
		Metadata: map[string]any{pluginMetadataKey: in.Metadata, "source": pluginMetadataKey},
	}
	if in.WorkflowStepID != nil {
		task.WorkflowStepID = *in.WorkflowStepID
	}
	r.host.tasks[task.ID] = task
	return task, nil
}

func (h *fakeHost) Sessions() pluginsdk.SessionReader {
	return fakeSessionReader{SessionReader: pluginsdk.UnimplementedHostData{}.Sessions(), host: h}
}

type fakeSessionReader struct {
	pluginsdk.SessionReader
	host *fakeHost
}

func (r fakeSessionReader) List(_ context.Context, filter pluginsdk.SessionFilter, _ pluginsdk.Page) ([]pluginsdk.Session, *pluginsdk.PageInfo, error) {
	var result []pluginsdk.Session
	for _, session := range r.host.sessions {
		for _, id := range filter.TaskIDs {
			if session.TaskID == id {
				result = append(result, session)
			}
		}
	}
	return result, nil, nil
}

func (r fakeTaskReader) Get(_ context.Context, id string) (*pluginsdk.Task, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	if r.host.getTaskErr != nil {
		return nil, r.host.getTaskErr
	}
	task, ok := r.host.tasks[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "fakeHost: task %s not found", id)
	}
	copyTask := *task
	return &copyTask, nil
}

type fakeTaskTreeManager struct {
	host *fakeHost
}

func (m fakeTaskTreeManager) Preview(_ context.Context, rootTaskID string) ([]pluginsdk.Task, error) {
	m.host.mu.Lock()
	defer m.host.mu.Unlock()
	task, ok := m.host.tasks[rootTaskID]
	if !ok {
		return nil, nil
	}
	return []pluginsdk.Task{*task}, nil
}

func (m fakeTaskTreeManager) Delete(_ context.Context, rootTaskID string) ([]string, error) {
	m.host.mu.Lock()
	defer m.host.mu.Unlock()
	if _, ok := m.host.tasks[rootTaskID]; !ok {
		return []string{}, nil
	}
	delete(m.host.tasks, rootTaskID)
	m.host.deleted = append(m.host.deleted, rootTaskID)
	return []string{rootTaskID}, nil
}

var (
	_ pluginsdk.Host                    = (*fakeHost)(nil)
	_ pluginsdk.PluginOwnedTaskTreeHost = (*fakeHost)(nil)
)
