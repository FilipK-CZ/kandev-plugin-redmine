import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";

const bundle = await readFile(new URL("./bundle.js", import.meta.url), "utf8");

async function loadBundleTestHooks() {
  globalThis.window = { registerKandevPlugin() {} };
  const source = `${bundle}\nglobalThis.__redmineBundleTestHooks = { createSyncSaveController, syncControllerForWorkspace, workspaceActionInvoke, taskActionInvoke, makeRedmineCommentAction, makeSettingsComponent };`;
  await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}`);
  return globalThis.__redmineBundleTestHooks;
}

test("mapping selects use host controls and explicitly added live statuses", () => {
  assert.match(bundle, /const unmappedValue = "__unmapped__"/);
  assert.match(bundle, /value === unmappedValue \? "" : value/);
  assert.match(bundle, /redmine-status-step-\$\{status\.id\}/);
  assert.match(bundle, /redmine-status-add-select/);
  assert.match(bundle, /redmine-status-add/);
  assert.match(bundle, /redmine-status-remove-\$\{status\.id\}/);
  assert.match(bundle, /Choose a workflow step for every added Redmine status/);
  assert.match(bundle, /task_priority: priorityMap\[priority\.id\] \|\| ""/);
  assert.match(bundle, /redmine-priority-map-" \+ priority\.id/);
  assert.match(bundle, /SelectTrigger, \{ "data-testid": "redmine-priority-map-" \+ priority\.id \}/);
  assert.doesNotMatch(bundle, /trackerLabels|task_label|redmine-tracker-label/);
});

async function commentDialog(invoke) {
  const { makeRedmineCommentAction } = await loadBundleTestHooks();
  let component;
  let cursor = 0;
  const hooks = [];
  const host = {
    jsx: (type, props, ...children) => ({ type, props: props || {}, children }),
    React: {
      useState(initial) { const index = cursor++; if (!(index in hooks)) hooks[index] = initial; return [hooks[index], value => { hooks[index] = value; }]; },
      useRef(initial) { const index = cursor++; if (!(index in hooks)) hooks[index] = { current: initial }; return hooks[index]; },
    },
    ui: { Button: "Button", Textarea: "Textarea", Label: "Label", Badge: "Badge" },
    api: { invokeAction: invoke },
    toast: { error(message) { throw new Error(message); } },
    openModal({ content }) { component = content; return { close() {} }; },
  };
  await makeRedmineCommentAction(host).run({ workspaceId: "workspace-1", taskId: "task-1" });
  const render = () => { cursor = 0; return component(); };
  const all = (node) => !node || typeof node !== "object" ? [] : [node, ...node.children.flat(Infinity).flatMap(all)];
  return { render, find: id => all(render()).find(node => node.props["data-testid"] === id), select: () => all(render()).find(node => node.type === "select") };
}

test("comment preview never publishes and approval uses the immutable draft id", async () => {
  const calls = [];
  let finish;
  const draft = { id: "draft-id", state: "prepared", notes: "Reviewed text", private_notes: false };
  const dialog = await commentDialog(async (key, context) => {
    calls.push({ key, context });
    if (key === "comments.get") return { issue: { id: 42, url: "https://redmine.example/issues/42", subject: "Example" }, current_id: "", context_hash: "context-hash" };
    if (key === "comments.prepare") return draft;
    if (key === "comments.approve") return new Promise(resolve => { finish = () => resolve({ ...draft, state: "published", journal_id: 123 }); });
    throw new Error(`unexpected action ${key}`);
  });
  assert.equal(dialog.find("redmine-comment-preview").props.disabled, true);
  dialog.find("redmine-comment-notes").props.onChange({ target: { value: "Reviewed text" } });
  dialog.select().props.onChange({ target: { value: "public" } });
  await dialog.find("redmine-comment-preview").props.onClick();
  assert.equal(calls.some(call => call.key === "comments.approve"), false);
  assert.equal(dialog.find("redmine-comment-preview-text").children[0], "Reviewed text");
  const approve = dialog.find("redmine-comment-approve").props.onClick;
  const pending = approve();
  await approve();
  assert.equal(calls.filter(call => call.key === "comments.approve").length, 1);
  assert.deepEqual(calls.at(-1).context, { workspaceId: "workspace-1", taskId: "task-1", body: { draft_id: "draft-id" } });
  finish(); await pending;
  assert.ok(dialog.find("redmine-comment-published"));
});

test("lost comment approval acknowledgement offers read-only reconciliation", async () => {
  const calls = [];
  const draft = { id: "draft-id", state: "prepared", notes: "Exact proposed text", private_notes: true };
  const dialog = await commentDialog(async (key) => {
    calls.push(key);
    if (key === "comments.get") return { issue: { id: 42 }, draft, current_id: "draft-id", can_approve: true };
    if (key === "comments.approve") throw new Error("Connection lost");
    if (key === "comments.reconcile") return { ...draft, state: "published", journal_id: 123 };
    throw new Error(`unexpected action ${key}`);
  });
  await dialog.find("redmine-comment-approve").props.onClick();
  assert.equal(dialog.find("redmine-comment-approve"), undefined);
  assert.ok(dialog.find("redmine-comment-reconcile"));
  await dialog.find("redmine-comment-reconcile").props.onClick();
  assert.ok(dialog.find("redmine-comment-published"));
  assert.deepEqual(calls, ["comments.get", "comments.approve", "comments.reconcile"]);
});

test("editing a prepared comment invalidates its approval before accepting edits", async () => {
  const calls = [];
  const draft = { id: "draft-id", state: "prepared", notes: "Original text", private_notes: false };
  const dialog = await commentDialog(async (key) => {
    calls.push(key);
    if (key === "comments.get") return { issue: { id: 42 }, draft, current_id: "draft-id", can_approve: true };
    if (key === "comments.discard") return { ...draft, state: "cancelled" };
    throw new Error(`unexpected action ${key}`);
  });
  await dialog.find("redmine-comment-edit").props.onClick();
  assert.ok(dialog.find("redmine-comment-notes"));
  assert.equal(dialog.find("redmine-comment-approve"), undefined);
  assert.deepEqual(calls, ["comments.get", "comments.get", "comments.discard"]);
});

async function replySettingsForm(autoPublish) {
  const { makeSettingsComponent } = await loadBundleTestHooks();
  const calls = [];
  let cursor = 0;
  let section = false;
  let hooks = [{ state: "connected" }, false];
  const effects = [];
  const host = {
    jsx: (type, props, ...children) => ({ type, props: props || {}, children }),
    ui: new Proxy({}, { get: (_, key) => key }),
    React: {
      useState(initial) { const index = cursor++; if (!(index in hooks)) hooks[index] = initial; return [hooks[index], value => { hooks[index] = value; }]; },
      useRef(initial) { const index = cursor++; if (!(index in hooks)) hooks[index] = { current: initial }; return hooks[index]; },
      useCallback(fn) { return fn; },
      useEffect(fn) { if (!section) return; const index = cursor++; if (!(index in hooks)) { hooks[index] = true; effects.push(fn); } },
    },
    toast: { success() {}, error(message) { throw new Error(message); } },
    api: { async invokeAction(key, context) {
      calls.push({ key, context });
      if (key === "replies.settings.get") return { workflow_id: "workflow-1", step_id: "reply-step", prompt_name: "client-reply", ...(autoPublish === undefined ? {} : { auto_publish: autoPublish }) };
      if (key === "workflows.list") return { workflows: [{ id: "workflow-1", name: "Example", steps: [{ id: "reply-step", name: "Client reply" }] }] };
      if (key === "replies.settings.save") return context.body;
      throw new Error(`unexpected action ${key}`);
    } },
  };
  const all = node => !node || typeof node !== "object" ? [] : [node, ...node.children.flat(Infinity).flatMap(all)];
  const page = makeSettingsComponent(host)({ workspaceId: "workspace-1" });
  const component = all(page).find(node => node.type?.name === "ReplyDraftingSection");
  assert.ok(component);
  section = true; hooks = [];
  const render = () => { cursor = 0; return component.type(component.props); };
  render(); effects.forEach(effect => effect());
  await new Promise(resolve => setImmediate(resolve));
  return { calls, find: id => all(render()).find(node => node.props["data-testid"] === id) };
}

test("reply publication defaults to human approval and saves either mode explicitly", async () => {
  const form = await replySettingsForm(undefined);
  assert.equal(form.find("redmine-reply-auto-publish").props.checked, false);
  form.find("redmine-reply-auto-publish").props.onCheckedChange(true);
  assert.equal(form.calls.some(call => call.key === "replies.settings.save"), false);
  await form.find("redmine-reply-save").props.onClick();
  assert.equal(form.calls.at(-1).context.body.auto_publish, true);
  assert.equal(form.calls.at(-1).context.workspaceId, "workspace-1");
  assert.equal("taskId" in form.calls.at(-1).context, false);
  form.find("redmine-reply-auto-publish").props.onCheckedChange(false);
  await form.find("redmine-reply-save").props.onClick();
  assert.equal(form.calls.at(-1).context.body.auto_publish, false);
});

test("reply publication loads the saved automatic mode without silently resetting it", async () => {
  const form = await replySettingsForm(true);
  assert.equal(form.find("redmine-reply-auto-publish").props.checked, true);
});

test("integration registration supplies a branded icon and workspace enable action", () => {
  assert.match(bundle, /icon: redmineIcon/);
  assert.match(bundle, /action: makeIntegrationEnabledAction\(host\)/);
  assert.match(bundle, /host\.ui\.IntegrationEnabledControl/);
  assert.match(bundle, /"integration.enabled.get"/);
  assert.match(bundle, /"integration.enabled.save"/);
});

test("watcher filters use a Redmine-style add/remove field picker", () => {
  assert.match(bundle, /redmine-watch-project/);
  assert.match(bundle, /redmine-watch-filter-add/);
  assert.match(bundle, /activeFilters/);
  assert.match(bundle, /Add the live Redmine fields you want to match/);
  assert.match(bundle, /redmine-watch-filter-operator-/);
  assert.match(bundle, /redmine-watch-filter-value-/);
  assert.match(bundle, /redmine-watch-filter-remove-/);
  assert.match(bundle, /filters: activeFilters/);
  assert.doesNotMatch(bundle, /h\("select", \{ id: "redmine-new-watch-/);
});

test("watcher configuration loads refreshable live project filters", () => {
  for (const id of ["redmine-watch-filters", "redmine-watch-filters-refresh", "redmine-watch-filter-add"]) assert.match(bundle, new RegExp(id));
  assert.match(bundle, /watches\.filter_options/);
  assert.match(bundle, /Choices are scoped to this project/);
});

test("project selection uses a dropdown with removable selected projects", () => {
  assert.match(bundle, /redmine-project-select/);
  assert.match(bundle, /redmine-selected-projects/);
  assert.match(bundle, /redmine-project-remove-\$\{id\}/);
  assert.match(bundle, /Select project/);
  assert.doesNotMatch(bundle, /h\(Checkbox, \{[\s\S]*redmine-project-/);
});

test("mapping renders custom field identifiers and derived-mode evidence", () => {
  assert.match(bundle, /redmine-custom-fields/);
  assert.match(bundle, /redmine-custom-field-" \+ field\.id/);
  assert.match(bundle, /#\$\{field\.id\} \$\{field\.name\}/);
  assert.match(bundle, /redmine-custom-fields-derived-note/);
});

test("primary task menu actions retain Link and use verified task actions", () => {
  assert.match(bundle, /registry\.registerTaskAction\(makeLinkTaskAction\(host\)\)/);
  assert.match(bundle, /registry\.registerTaskMenuAction\(makeSetRedmineStatusAction\(host\)\)/);
  assert.match(bundle, /registry\.registerTaskMenuAction\(makeUnlinkRedmineAction\(host\)\)/);
  assert.match(bundle, /group: "primary"/);
  for (const key of ["link.get", "fieldmapping.get", "link.set_status", "link.unset"]) assert.match(bundle, new RegExp(`"${key}"`));
  for (const id of ["redmine-status-modal", "redmine-status-picker", "redmine-status-confirm", "redmine-unlink-modal", "redmine-unlink-confirm"]) assert.match(bundle, new RegExp(id));
});

test("manual status uses workspace scope for field mappings and task scope for links", async () => {
  let registration;
  globalThis.window = {
    registerKandevPlugin(_id, value) {
      registration = value;
    },
  };
  await import(new URL(`./bundle.js?manual-status-contract=${Date.now()}`, import.meta.url));

  const menuActions = [];
  const calls = [];
  const host = {
    jsx: () => null,
    React: {},
    ui: {},
    toast: { success() {}, error() {} },
    openModal() { return { close() {} }; },
    api: {
      async invokeAction(key, context) {
        calls.push({ key, context });
        if (key === "link.get") return { linked: true };
        if (key === "fieldmapping.get") return { live_statuses: [] };
        throw new Error(`unexpected action ${key}`);
      },
    },
  };
  registration.initialize({ registerTaskAction() {}, registerTaskMenuAction(action) { menuActions.push(action); }, registerIntegrationSettings() {} }, host);
  const action = menuActions.find((candidate) => candidate.id === "redmine-set-status");
  await action.run({ workspaceId: "workspace-1", taskId: "task-1" });

  assert.deepEqual(calls, [
    { key: "link.get", context: { workspaceId: "workspace-1", taskId: "task-1", body: {} } },
    { key: "fieldmapping.get", context: { workspaceId: "workspace-1", body: {} } },
  ]);
});

test("sync saves roll back and toast without an unhandled rejection", async () => {
  const { createSyncSaveController } = await loadBundleTestHooks();
  const applied = [];
  const toasts = [];
  const controller = createSyncSaveController(
    async () => { throw new Error("save rejected"); },
    "workspace-1",
    { error(message) { toasts.push(message); } },
    (next) => applied.push(next),
    () => {},
  );
  controller.setOptions({ autoStatusWriteback: false, syncTitleDescription: true });
  assert.equal(await controller.update("autoStatusWriteback", true), false);
  assert.deepEqual(applied, [
    { autoStatusWriteback: true, syncTitleDescription: true },
    { autoStatusWriteback: false, syncTitleDescription: true },
  ]);
  assert.deepEqual(toasts, ["save rejected"]);
});

test("sync saves are serialized and use workspace-only action context", async () => {
  const { createSyncSaveController, workspaceActionInvoke } = await loadBundleTestHooks();
  let release;
  const pending = new Promise((resolve) => { release = resolve; });
  const calls = [];
  const controller = createSyncSaveController(
    async (key, workspaceId, body) => { calls.push({ key, workspaceId, body }); await pending; },
    "workspace-1",
    { error() {} },
    () => {},
    () => {},
  );
  const first = controller.update("autoStatusWriteback", true);
  assert.equal(await controller.update("syncTitleDescription", true), false);
  release();
  assert.equal(await first, true);
  assert.deepEqual(calls, [{ key: "syncoptions.save", workspaceId: "workspace-1", body: { auto_status_writeback: true, sync_title_description: false } }]);

  const contexts = [];
  await workspaceActionInvoke({ api: { invokeAction: async (_key, context) => { contexts.push(context); return {}; } } }, "projects.save", { workspaceId: "workspace-1", taskId: "must-not-send" }, {});
  assert.deepEqual(contexts, [{ workspaceId: "workspace-1", body: {} }]);
});

test("sync controller is recreated when the settings workspace changes", async () => {
  const { syncControllerForWorkspace } = await loadBundleTestHooks();
  const ref = { current: null };
  const calls = [];
  const make = (workspaceId) => syncControllerForWorkspace(
    ref,
    async (key, targetWorkspaceId, body) => { calls.push({ key, workspaceId: targetWorkspaceId, body }); },
    workspaceId,
    { error() {} },
    () => {},
    () => {},
  );
  const first = make("workspace-1");
  await first.update("autoStatusWriteback", true);
  const second = make("workspace-2");
  await second.update("syncTitleDescription", true);
  assert.notEqual(first, second);
  assert.deepEqual(calls.map((call) => call.workspaceId), ["workspace-1", "workspace-2"]);
});
