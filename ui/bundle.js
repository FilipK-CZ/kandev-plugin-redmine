// kandev plugin UI bundle — the frontend half of kandev-plugin-redmine.
//
// This is a hand-written, NO-BUILD plain-JS ES module. It ships byte-for-byte
// inside the package tar.gz under ui/bundle.js, and kandev serves it directly
// from the extracted package at GET /api/plugins/<id>/ui/bundle.js, then
// dynamically imports it as a native ES module. There is nothing to build:
// edit this file and repackage (`make package` / `make package-host`).
//
// The contract, in three touch points:
//   - window.registerKandevPlugin(id, { initialize, destroy }) is the single
//     global entry point the host calls once this module has been evaluated.
//     `id` MUST match manifest.yaml's id.
//   - The `host` object handed to initialize() carries the SHARED host React
//     instance (`host.React`, `host.jsx` == host.React.createElement) plus a
//     curated design system (`host.ui`), imperative toasts (`host.toast`),
//     shared helpers (`host.utils`), navigation (`host.navigate`), the task
//     Link dialog (`host.openTaskLinkDialog`), and authenticated backend
//     calls (`host.api.invokeAction`). NEVER import or bundle your own React
//     — that breaks hook identity across the host tree. The same goes for
//     recharts: use the `host.ui.Chart*` wrappers.
//   - `registry` is where nav items, routes, integration settings pages, task
//     actions, and slot components are declared. Every registration is
//     tracked under this plugin's id, so the host bulk-unregisters everything
//     when the plugin is disabled.
//
// Composer `#` mentions (reference_sources in manifest.yaml) have no
// frontend registration call — they are purely manifest-declared plus the
// backend SearchEntityReferences/AuthorizeEntityReference implementation
// (server/references.go). There is nothing to add here for that surface.

// ---------------------------------------------------------------------------
// Task linking — registerTaskAction({placement:"link"}) opens the shared
// host Link dialog. The plugin supplies only copy strings and an
// onSubmit(reference, signal) callback; host.openTaskLinkDialog is a single
// free-text input (an issue ID, "#123" shorthand, or issue URL), not a
// search/picker UI. Resolving that reference to a real issue and persisting
// the link happen entirely backend-side, via the link.set action
// (server/actions_link.go's parseIssueReference + GetIssue call).
// ---------------------------------------------------------------------------
function makeLinkTaskAction(host) {
  return {
    id: "redmine-link",
    label: "Redmine Issue",
    placement: "link",
    singleTaskOnly: true,
    async run(context) {
      host.openTaskLinkDialog({
        title: "Link Redmine issue",
        description: "Enter a Redmine issue ID, \"#123\", or an issue URL.",
        inputLabel: "Issue",
        placeholder: "#123 or https://redmine.example.com/issues/123",
        emptyError: "Enter an issue ID or URL.",
        failureMessage: "Could not link that Redmine issue. Check the ID and try again.",
        successMessage: "Linked to Redmine issue.",
        inputTestId: "redmine-link-input",
        errorTestId: "redmine-link-error",
        submitTestId: "redmine-link-submit",
        async onSubmit(reference, signal) {
          await host.api.invokeAction(
            "link.set",
            {
              workspaceId: context.workspaceId,
              taskId: context.taskId,
              body: { reference },
            },
            { signal },
          );
        },
      });
    },
  };
}

function actionInvoke(host, key, actionContext, body) {
  return host.api.invokeAction(key, { ...actionContext, body }).then((result) => {
    if (result && result.kind) throw new Error(result.error || "Redmine action failed.");
    return result;
  });
}

function workspaceActionInvoke(host, key, context, body) {
  return actionInvoke(host, key, { workspaceId: context.workspaceId }, body);
}

function taskActionInvoke(host, key, context, body) {
  return actionInvoke(host, key, { workspaceId: context.workspaceId, taskId: context.taskId }, body);
}

function makeRedmineIcon(host) {
  return function RedmineIcon({ className }) {
    return host.jsx(
      "svg",
      { className, viewBox: "0 0 24 24", "aria-hidden": true, focusable: false, fill: "currentColor" },
      host.jsx("path", { d: "M4 3h9a6 6 0 0 1 2.1 11.62L21 21h-5.45l-5.05-6H9v6H4V3Zm5 4v4h4a2 2 0 1 0 0-4H9Z" }),
    );
  };
}

function makeIntegrationEnabledAction(host) {
  return function RedmineIntegrationEnabledAction({ workspaceId }) {
    const React = host.React;
    const [enabled, setEnabled] = React.useState(null);
    React.useEffect(() => {
      let active = true;
      if (!workspaceId) { setEnabled(null); return () => { active = false; }; }
      workspaceActionInvoke(host, "integration.enabled.get", { workspaceId }, {}).then((result) => {
        if (active) setEnabled(result.enabled !== false);
      }).catch((err) => host.toast.error(err.message || String(err)));
      return () => { active = false; };
    }, [workspaceId]);
    if (!workspaceId || enabled === null) return null;
    return host.jsx(host.ui.IntegrationEnabledControl, {
      id: "redmine",
      name: "Redmine",
      enabled,
      persist: async (nextEnabled) => {
        await workspaceActionInvoke(host, "integration.enabled.save", { workspaceId }, { enabled: nextEnabled });
        setEnabled(nextEnabled);
      },
    });
  };
}

// The controller serializes option writes. Keeping this outside the React
// component makes its failure rollback behavior testable without a browser.
function createSyncSaveController(invoke, workspaceId, toast, apply, setSaving) {
  let options = { autoStatusWriteback: false, syncTitleDescription: false };
  let pending = false;
  return {
    setOptions(next) { options = next; },
    async update(key, value) {
      if (pending) return false;
      const prior = options;
      const next = { ...prior, [key]: value };
      pending = true;
      setSaving(true);
      options = next;
      apply(next);
      try {
        await invoke("syncoptions.save", workspaceId, {
          auto_status_writeback: next.autoStatusWriteback,
          sync_title_description: next.syncTitleDescription,
        });
        return true;
      } catch (err) {
        options = prior;
        apply(prior);
        toast.error(err && err.message ? err.message : String(err));
        return false;
      } finally {
        pending = false;
        setSaving(false);
      }
    },
  };
}

function syncControllerForWorkspace(ref, invoke, workspaceId, toast, apply, setSaving) {
  if (!ref.current || ref.current.workspaceId !== workspaceId) {
    ref.current = { workspaceId, controller: createSyncSaveController(invoke, workspaceId, toast, apply, setSaving) };
  }
  return ref.current.controller;
}

function makeSetRedmineStatusAction(host) {
  const h = host.jsx;
  return {
    id: "redmine-set-status", label: "Set Redmine status", group: "primary", singleTaskOnly: true,
    async run(context) {
      try {
        const link = await taskActionInvoke(host, "link.get", context, {});
        if (!link.linked) throw new Error("Link this task to a Redmine issue first.");
        const fields = await workspaceActionInvoke(host, "fieldmapping.get", context, {});
        const statuses = fields.live_statuses || [];
        const closeRef = {};
        function StatusModal() {
          const React = host.React;
          const [statusId, setStatusId] = React.useState("");
          const [error, setError] = React.useState(null);
          const submit = async () => {
            const id = Number(statusId);
            if (!Number.isSafeInteger(id) || id <= 0) { setError("Select a Redmine status."); return; }
            try { await taskActionInvoke(host, "link.set_status", context, { status_id: id }); host.toast.success("Redmine status updated."); closeRef.close && closeRef.close(); }
            catch (err) { setError(err.message || String(err)); }
          };
          return h("div", { "data-testid": "redmine-status-modal" },
            h("label", { htmlFor: "redmine-status-picker" }, "Redmine status"),
            h("select", { id: "redmine-status-picker", "data-testid": "redmine-status-picker", value: statusId, onChange: (event) => setStatusId(event.target.value) }, [h("option", { value: "" }, "Select status")].concat(statuses.map((status) => h("option", { key: status.id, value: status.id }, status.name)))),
            error ? h("p", { "data-testid": "redmine-status-error" }, error) : null,
            h("button", { type: "button", "data-testid": "redmine-status-confirm", onClick: submit }, "Update status"),
          );
        }
        const handle = host.openModal({ title: "Set Redmine status", size: "sm", content: StatusModal });
        closeRef.close = handle.close;
      } catch (err) { host.toast.error(err.message || String(err)); }
    },
  };
}

function makeRedmineCommentAction(host) {
  const h = host.jsx;
  return {
    id: "redmine-comment", label: "Review Redmine reply", group: "primary", singleTaskOnly: true,
    async run(context) {
      try {
        const initial = await taskActionInvoke(host, "comments.get", context, {});
        const closeRef = {};
        function CommentModal() {
          const React = host.React;
          const { Button, Textarea, Label, Badge } = host.ui;
          const [review, setReview] = React.useState(initial.draft?.state === "prepared" && !initial.can_approve ? null : initial.draft);
          const [currentID, setCurrentID] = React.useState(initial.current_id || "");
          const [issue, setIssue] = React.useState(initial.issue);
          const [contextHash, setContextHash] = React.useState(initial.context_hash);
          const [notes, setNotes] = React.useState(initial.draft?.notes || initial.proposal?.notes || "");
          const [visibility, setVisibility] = React.useState(initial.draft ? (initial.draft.private_notes ? "private" : "public") : initial.proposal ? (initial.proposal.private_notes ? "private" : "public") : "");
          const [error, setError] = React.useState(null);
          const [busy, setBusy] = React.useState(false);
          const [uncertain, setUncertain] = React.useState(false);
          const [draftRequested, setDraftRequested] = React.useState(false);
          const inFlight = React.useRef(false);
          const run = async (action) => {
            if (inFlight.current) return;
            inFlight.current = true; setBusy(true); setError(null);
            try { await action(); }
            catch (err) { setError(err.message || String(err)); }
            finally { inFlight.current = false; setBusy(false); }
          };
          const prepare = () => run(async () => {
            const value = await taskActionInvoke(host, "comments.prepare", context, { issue_id: issue.id, notes, private_notes: visibility === "private", expected_id: currentID, expected_context_hash: contextHash });
            setReview(value); setCurrentID(value.id); setUncertain(false);
          });
          const publish = () => run(async () => {
            setUncertain(true);
            const value = await taskActionInvoke(host, "comments.approve", context, { draft_id: review.id });
            setReview(value); setUncertain(false);
          });
          const requestDraft = () => run(async () => {
            const result = await taskActionInvoke(host, "replies.request", context, { request_id: globalThis.crypto.randomUUID() });
            setDraftRequested(result.state !== "denied");
            setError(result.state === "denied" ? "Draft request denied. Grant the plugin workspace message access before trying again." : result.state === "uncertain" ? "Draft dispatch is unconfirmed. Inspect the native task; do not repeat the request." : "Draft requested. Check the task conversation, then reopen Review Redmine reply to load it.");
          });
          const reconcile = () => run(async () => {
            const value = await taskActionInvoke(host, "comments.reconcile", context, { draft_id: review.id });
            setReview(value); setUncertain(false);
          });
          const edit = () => run(async () => {
            const latest = await taskActionInvoke(host, "comments.get", context, {});
            if (review?.state === "prepared") await taskActionInvoke(host, "comments.discard", context, { draft_id: review.id });
            if (review?.state === "published") { setNotes(""); setVisibility(""); }
            setIssue(latest.issue); setContextHash(latest.context_hash); setCurrentID(latest.current_id || "");
            setReview(null); setUncertain(false);
          });
          const pending = uncertain || review?.state === "sending" || review?.state === "unverified";
          return h("div", { className: "space-y-4", "data-testid": "redmine-comment-modal" },
            h("a", { href: review?.issue_url || issue.url, target: "_blank", rel: "noopener noreferrer", className: "font-medium underline" }, `#${issue.id} · ${review?.subject || issue.subject}`),
            h("p", { className: "text-sm text-muted-foreground" }, "Review the exact text and visibility. Only approval below publishes it; status and other issue fields stay unchanged."),
            initial.draft_warning ? h("p", { className: "text-sm text-muted-foreground" }, initial.draft_warning) : null,
            !review ? h("div", { className: "space-y-3" },
              h(Button, { variant: "outline", disabled: busy || draftRequested || initial.draft_request?.state === "uncertain" || initial.draft_request?.state === "sending" || (initial.draft_request?.state === "sent" && !initial.proposal), onClick: requestDraft, "data-testid": "redmine-reply-request" }, "Generate from current issue"),
              h(Label, { htmlFor: "redmine-comment-notes" }, "Proposed reply"),
              h(Textarea, { id: "redmine-comment-notes", "data-testid": "redmine-comment-notes", value: notes, maxLength: 32768, rows: 10, disabled: busy, onChange: event => setNotes(event.target.value) }),
              h(Label, { htmlFor: "redmine-comment-visibility" }, "Visibility"),
              h("select", { id: "redmine-comment-visibility", value: visibility, disabled: busy, onChange: event => setVisibility(event.target.value), className: "border rounded px-3 py-2 w-full" },
                h("option", { value: "" }, "Choose visibility"), h("option", { value: "public" }, "Public comment — visible to the client"), h("option", { value: "private" }, "Private internal note")),
              h(Button, { disabled: busy || !notes.trim() || !visibility, onClick: prepare, "data-testid": "redmine-comment-preview" }, "Review exact comment"),
            ) : h("div", { className: "space-y-3" },
              h(Badge, { variant: review.private_notes ? "secondary" : "default", "data-testid": "redmine-comment-visibility-badge" }, review.private_notes ? "Private internal note" : "Public — visible to the client"),
              h("div", { className: "whitespace-pre-wrap break-words rounded border p-3", "data-testid": "redmine-comment-preview-text" }, review.notes),
              review.state === "published" ? h("p", { "data-testid": "redmine-comment-published" }, `Published and verified · journal #${review.journal_id}`) : null,
              review.state === "rejected" ? h("p", { role: "alert" }, "Redmine rejected the comment. Review access and prepare a new preview; it will not retry automatically.") : null,
              pending ? h("div", null,
                h("p", { role: "alert" }, "Publication is not confirmed. Check the result; do not create or send another comment."),
                h(Button, { disabled: busy, onClick: reconcile, "data-testid": "redmine-comment-reconcile" }, "Check publication result")) : null,
              review.state === "prepared" && !pending ? h(Button, { disabled: busy, onClick: publish, "data-testid": "redmine-comment-approve" }, review.private_notes ? "Approve and publish private note" : "Approve and publish public comment") : null,
              !pending ? h(Button, { variant: "outline", disabled: busy, onClick: edit, "data-testid": "redmine-comment-edit" }, review.state === "published" ? "New comment" : "Edit and review again") : null,
            ),
            error ? h("p", { role: "alert", "data-testid": "redmine-comment-error" }, error) : null,
            h(Button, { variant: "outline", onClick: () => closeRef.close?.() }, "Close"),
          );
        }
        const handle = host.openModal({ title: "Review Redmine reply", size: "lg", content: CommentModal });
        closeRef.close = handle.close;
      } catch (err) { host.toast.error(err.message || String(err)); }
    },
  };
}

function makeUnlinkRedmineAction(host) {
  const h = host.jsx;
  return {
    id: "redmine-unlink", label: "Unlink Redmine issue", group: "primary", singleTaskOnly: true,
    async run(context) {
      try {
        const link = await taskActionInvoke(host, "link.get", context, {});
        if (!link.linked) throw new Error("This task is not linked to a Redmine issue.");
        const closeRef = {};
        function UnlinkModal() {
          const [error, setError] = host.React.useState(null);
          const confirm = async () => { try { await taskActionInvoke(host, "link.unset", context, {}); host.toast.success("Redmine issue unlinked."); closeRef.close && closeRef.close(); } catch (err) { setError(err.message || String(err)); } };
          return h("div", { "data-testid": "redmine-unlink-modal" }, h("p", null, "Unlink this task from Redmine?"), error ? h("p", { "data-testid": "redmine-unlink-error" }, error) : null, h("button", { type: "button", "data-testid": "redmine-unlink-confirm", onClick: confirm }, "Unlink"));
        }
        const handle = host.openModal({ title: "Unlink Redmine issue", size: "sm", content: UnlinkModal });
        closeRef.close = handle.close;
      } catch (err) { host.toast.error(err.message || String(err)); }
    },
  };
}

// ---------------------------------------------------------------------------
// Settings page — registerIntegrationSettings. Built from independent
// sections (Connection, Projects, Field mapping, Sync options, Watchers) so
// each can be read/edited without unpicking the others. Every section calls
// host.api.invokeAction directly against the actions declared in
// manifest.yaml; there is no client-side caching layer, so a save always
// re-fetches to reflect exactly what the backend now has stored.
// ---------------------------------------------------------------------------
function makeSettingsComponent(host) {
  const { jsx: h, ui, toast } = host;
  const {
    Card, CardHeader, CardTitle, CardDescription, CardContent, CardFooter,
    Button, Input, Label, Badge, Switch,
    Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
    Table, TableHeader, TableBody, TableRow, TableHead, TableCell,
    Empty, EmptyHeader, EmptyTitle, EmptyDescription,
    Spinner,
  } = ui;

  async function invoke(key, workspaceId, body) {
    const result = await host.api.invokeAction(key, { workspaceId, body });
    if (result && result.kind) {
      throw new Error(result.error || "Redmine action failed.");
    }
    return result;
  }

  function errorMessage(err) {
    return err && err.message ? err.message : String(err);
  }

  // -- Connection ------------------------------------------------------
  function ConnectionSection({ workspaceId, connection, reload }) {
    const React = host.React;
    const [baseUrl, setBaseUrl] = React.useState(connection.base_url || "");
    const [apiKey, setApiKey] = React.useState("");
    const [saving, setSaving] = React.useState(false);
    const [error, setError] = React.useState(null);

    const onSave = async () => {
      setSaving(true);
      setError(null);
      try {
        const result = await invoke("connection.save", workspaceId, { base_url: baseUrl, api_key: apiKey });
        if (result && result.kind) {
          setError(result.error || "Could not connect to Redmine.");
          return;
        }
        toast.success("Connected to Redmine.");
        setApiKey("");
        await reload();
      } catch (err) {
        setError(errorMessage(err));
      } finally {
        setSaving(false);
      }
    };

    const onDisconnect = async () => {
      setSaving(true);
      try {
        await invoke("connection.disconnect", workspaceId, {});
        toast.success("Disconnected from Redmine.");
        await reload();
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setSaving(false);
      }
    };

    const badgeVariant =
      connection.state === "connected" ? "default" : connection.state === "degraded" ? "destructive" : "secondary";

    return h(
      Card,
      { id: "redmine-connection-card" },
      h(
        CardHeader,
        null,
        h(CardTitle, null, "Connection"),
        h(CardDescription, null, "Connect a Redmine instance with an API key from your Redmine account's My account page."),
      ),
      h(
        CardContent,
        { className: "space-y-4" },
        h(
          "div",
          { className: "flex items-center gap-2" },
          h(Badge, { variant: badgeVariant, id: "redmine-connection-state" }, connection.state),
          connection.last_error
            ? h("span", { className: "text-destructive text-xs", id: "redmine-connection-last-error" }, connection.last_error)
            : null,
        ),
        h(
          "div",
          { className: "space-y-2" },
          h(Label, { htmlFor: "redmine-base-url" }, "Base URL"),
          h(Input, {
            id: "redmine-base-url",
            "data-testid": "redmine-base-url-input",
            placeholder: "https://redmine.example.com",
            value: baseUrl,
            onChange: (e) => setBaseUrl(e.target.value),
          }),
        ),
        h(
          "div",
          { className: "space-y-2" },
          h(Label, { htmlFor: "redmine-api-key" }, "API key"),
          h(Input, {
            id: "redmine-api-key",
            "data-testid": "redmine-api-key-input",
            type: "password",
            placeholder: connection.state === "connected" ? "Leave blank to keep the current key" : "",
            value: apiKey,
            onChange: (e) => setApiKey(e.target.value),
          }),
        ),
        error ? h("p", { className: "text-destructive text-sm", id: "redmine-connection-error" }, error) : null,
      ),
      h(
        CardFooter,
        { className: "gap-2" },
        h(
          Button,
          { id: "redmine-connection-save", "data-testid": "redmine-connection-save", disabled: saving || !baseUrl || (!apiKey && connection.state !== "connected"), onClick: onSave },
          "Save",
        ),
        connection.state !== "disconnected"
          ? h(Button, { variant: "outline", disabled: saving, onClick: onDisconnect }, "Disconnect")
          : null,
      ),
    );
  }

  // -- Projects ----------------------------------------------------------
  function ProjectsSection({ workspaceId, connected }) {
    const React = host.React;
    const [loading, setLoading] = React.useState(true);
    const [projects, setProjects] = React.useState([]);
    const [selected, setSelected] = React.useState(new Set());
    const [projectToAdd, setProjectToAdd] = React.useState("");
    const [saving, setSaving] = React.useState(false);

    const load = React.useCallback(async () => {
      if (!connected) {
        setLoading(false);
        return;
      }
      setLoading(true);
      try {
        const result = await invoke("projects.list", workspaceId, {});
        setProjects((result && result.projects) || []);
        setSelected(new Set((result && result.selected_ids) || []));
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setLoading(false);
      }
    }, [workspaceId, connected]);

    React.useEffect(() => {
      load();
    }, [load]);

    if (!connected) return null;
    if (loading) return h(Spinner, { id: "redmine-projects-loading" });

    const addProject = (value) => {
      if (value === "__select_project__") {
        setProjectToAdd("");
        return;
      }
      const id = Number(value);
      if (!Number.isSafeInteger(id) || id <= 0) return;
      const next = new Set(selected);
      next.add(id);
      setSelected(next);
      setProjectToAdd("");
    };

    const removeProject = (id) => {
      const next = new Set(selected);
      next.delete(id);
      setSelected(next);
    };

    const onSave = async () => {
      if (saving) return;
      setSaving(true);
      try {
        await invoke("projects.save", workspaceId, { project_ids: Array.from(selected) });
        toast.success("Project selection saved.");
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setSaving(false);
      }
    };

    return h(
      Card,
      { id: "redmine-projects-card" },
      h(CardHeader, null, h(CardTitle, null, "Projects"), h(CardDescription, null, "Select which Redmine projects sync.")),
      h(
        CardContent,
        null,
        projects.length === 0
          ? h(Empty, { id: "redmine-projects-empty" }, h(EmptyHeader, null, h(EmptyTitle, null, "No projects found")))
          : h(
              "div",
              { className: "space-y-3" },
              h(
                "div",
                { className: "space-y-2" },
                h(Label, { htmlFor: "redmine-project-select" }, "Redmine project"),
                h(
                  Select,
                  { value: projectToAdd || "__select_project__", onValueChange: addProject },
                  h(SelectTrigger, { id: "redmine-project-select", "data-testid": "redmine-project-select", className: "w-full" }, h(SelectValue, { placeholder: "Select project" })),
                  h(SelectContent, null, [
                    h(SelectItem, { key: "__select_project__", value: "__select_project__" }, "Select project"),
                  ].concat(projects.map((project) => h(SelectItem, { key: project.id, value: String(project.id), disabled: selected.has(project.id) }, project.name)))),
                ),
              ),
              selected.size === 0
                ? h("p", { className: "text-muted-foreground text-sm", "data-testid": "redmine-projects-none" }, "No projects selected.")
                : h(
                    "ul",
                    { className: "space-y-2", "data-testid": "redmine-selected-projects" },
                    Array.from(selected).map((id) => {
                      const project = projects.find((candidate) => candidate.id === id);
                      return h(
                        "li",
                        { key: id, className: "flex items-center justify-between gap-2" },
                        h("span", null, project ? project.name : id),
                        h(Button, { type: "button", variant: "ghost", size: "sm", "data-testid": `redmine-project-remove-${id}`, onClick: () => removeProject(id) }, "Remove"),
                      );
                    }),
                  ),
            ),
      ),
      h(CardFooter, null, h(Button, { id: "redmine-projects-save", "data-testid": "redmine-projects-save", disabled: saving, onClick: onSave }, saving ? "Saving…" : "Save projects")),
    );
  }

  // -- Field mapping -------------------------------------------------------
  function FieldMappingSection({ workspaceId, connected }) {
    const React = host.React;
    const unmappedValue = "__unmapped__";
    const [loading, setLoading] = React.useState(true);
    const [live, setLive] = React.useState(null);
    const [workflows, setWorkflows] = React.useState([]);
    const [workflowId, setWorkflowId] = React.useState("");
    const [statusIDs, setStatusIDs] = React.useState([]);
    const [statusToAdd, setStatusToAdd] = React.useState("");
    const [statusSteps, setStatusSteps] = React.useState({});
    const [priorityMap, setPriorityMap] = React.useState({});
    const [saving, setSaving] = React.useState(false);

    const load = React.useCallback(async () => {
      if (!connected) {
        setLoading(false);
        return;
      }
      setLoading(true);
      try {
        const [fields, wf] = await Promise.all([
          invoke("fieldmapping.get", workspaceId, {}),
          invoke("workflows.list", workspaceId, {}),
        ]);
        setLive(fields);
        const availableWorkflows = (wf && wf.workflows) || [];
        setWorkflows(availableWorkflows);
        setWorkflowId(fields.workflow_id || (availableWorkflows[0] && availableWorkflows[0].id) || "");

        const steps = {};
        (fields.statuses || []).forEach((s) => {
          steps[s.redmine_status_id] = s.workflow_step_id;
        });
        setStatusSteps(steps);
        setStatusIDs((fields.statuses || []).filter((status) => status.workflow_step_id).map((status) => status.redmine_status_id));

        const priorities = {};
        (fields.priorities || []).forEach((priority) => {
          priorities[priority.redmine_priority_id] = priority.task_priority;
        });
        setPriorityMap(priorities);

      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setLoading(false);
      }
    }, [workspaceId, connected]);

    React.useEffect(() => {
      load();
    }, [load]);

    if (!connected) return null;
    if (loading || !live) return h(Spinner, { id: "redmine-fieldmapping-loading" });

    const selectedWorkflow = workflows.find((wf) => wf.id === workflowId);
    const allSteps = (selectedWorkflow && selectedWorkflow.steps) || [];
    const mappedStatuses = statusIDs.map((id) => (live.live_statuses || []).find((status) => status.id === id)).filter(Boolean);
    const availableStatuses = (live.live_statuses || []).filter((status) => !statusIDs.includes(status.id));

    const addStatus = () => {
      const id = Number(statusToAdd);
      if (!Number.isSafeInteger(id) || id <= 0 || statusIDs.includes(id)) return;
      setStatusIDs([...statusIDs, id]);
      setStatusToAdd("");
    };

    const removeStatus = (id) => {
      setStatusIDs(statusIDs.filter((statusID) => statusID !== id));
      setStatusSteps((current) => {
        const next = { ...current };
        delete next[id];
        return next;
      });
    };

    const onSave = async () => {
      if (saving) return;
      if (statusIDs.some((id) => !statusSteps[id])) {
        toast.error("Choose a workflow step for every added Redmine status.");
        return;
      }
      const statuses = mappedStatuses.map((s) => ({
        redmine_status_id: s.id,
        redmine_name: s.name,
        is_closed: s.is_closed,
        workflow_step_id: statusSteps[s.id] || "",
      }));
      const priorities = (live.live_priorities || []).map((priority) => ({
        redmine_priority_id: priority.id,
        redmine_name: priority.name,
        task_priority: priorityMap[priority.id] || "",
      }));
      setSaving(true);
      try {
        await invoke("fieldmapping.save", workspaceId, { workflow_id: workflowId, statuses, priorities });
        toast.success("Field mapping saved.");
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setSaving(false);
      }
    };

    return h(
      Card,
      { id: "redmine-fieldmapping-card" },
      h(
        CardHeader,
        null,
        h(CardTitle, null, "Field mapping"),
        h(CardDescription, null, "Map live Redmine statuses and priorities to Kandev — nothing here is hardcoded."),
      ),
      h(
        CardContent,
        { className: "space-y-6" },
        h("div", { className: "space-y-2" },
          h(Label, { htmlFor: "redmine-mapping-workflow" }, "Kandev workflow"),
          h(Select, { value: workflowId, onValueChange: (nextWorkflowId) => {
            const nextWorkflow = workflows.find((workflow) => workflow.id === nextWorkflowId);
            const allowedStepIDs = new Set(((nextWorkflow && nextWorkflow.steps) || []).map((step) => step.id));
            setWorkflowId(nextWorkflowId);
            setStatusSteps((current) => Object.fromEntries(Object.entries(current).map(([statusID, stepID]) => [statusID, allowedStepIDs.has(stepID) ? stepID : ""])));
          } },
            h(SelectTrigger, { id: "redmine-mapping-workflow", "data-testid": "redmine-mapping-workflow", className: "w-full" }, h(SelectValue, { placeholder: "Select workflow" })),
            h(SelectContent, null, workflows.map((workflow) => h(SelectItem, { key: workflow.id, value: workflow.id }, workflow.name))),
          ),
        ),
        h(
          "div",
          { className: "space-y-3" },
          h("h4", { className: "mb-2 text-sm font-medium" }, "Statuses → workflow step"),
          mappedStatuses.length ? h(
            Table,
            { id: "redmine-status-mapping-table" },
            h(TableHeader, null, h(TableRow, null, h(TableHead, null, "Redmine status"), h(TableHead, null, "Workflow step"), h(TableHead, { className: "w-20" }, ""))),
            h(
              TableBody,
              null,
              mappedStatuses.map((status) =>
                h(
                  TableRow,
                  { key: status.id },
                  h(TableCell, null, status.name, status.is_closed ? h(Badge, { variant: "secondary", className: "ml-2" }, "closed") : null),
                  h(
                    TableCell,
                    null,
                    h(
                      Select,
                      {
                        value: statusSteps[status.id] || unmappedValue,
                        onValueChange: (value) => setStatusSteps({ ...statusSteps, [status.id]: value === unmappedValue ? "" : value }),
                      },
                      h(SelectTrigger, { "data-testid": `redmine-status-step-${status.id}` }, h(SelectValue, { placeholder: "Unmapped" })),
                      h(
                        SelectContent,
                        null,
                        [h(SelectItem, { key: unmappedValue, value: unmappedValue }, "Unmapped")].concat(allSteps.map((step) => h(SelectItem, { key: step.id, value: step.id }, step.name))),
                      ),
                    ),
                  ),
                  h(TableCell, null, h(Button, { type: "button", variant: "ghost", size: "sm", "data-testid": `redmine-status-remove-${status.id}`, onClick: () => removeStatus(status.id) }, "Remove")),
                ),
              ),
            ),
          ) : h("p", { className: "text-muted-foreground text-sm", "data-testid": "redmine-status-mapping-empty" }, "No Redmine statuses mapped yet."),
          h("div", { className: "flex items-end gap-2" },
            h("div", { className: "min-w-0 flex-1 space-y-2" },
              h(Label, null, "Add Redmine status"),
              h(Select, { value: statusToAdd || "__select_status__", onValueChange: (value) => setStatusToAdd(value === "__select_status__" ? "" : value) },
                h(SelectTrigger, { "data-testid": "redmine-status-add-select", className: "w-full" }, h(SelectValue, { placeholder: "Select a live Redmine status" })),
                h(SelectContent, null, [h(SelectItem, { key: "__select_status__", value: "__select_status__" }, "Select a live Redmine status")].concat(availableStatuses.map((status) => h(SelectItem, { key: status.id, value: String(status.id) }, status.name)))),
              ),
            ),
            h(Button, { type: "button", variant: "outline", "data-testid": "redmine-status-add", disabled: !statusToAdd, onClick: addStatus }, "Add status"),
          ),
        ),
        h(
          "div",
          null,
          h("h4", { className: "mb-2 text-sm font-medium" }, "Priorities → task priority"),
          h(
            "div",
            { className: "space-y-2" },
            (live.live_priorities || []).map((priority) =>
              h(
                "div",
                { key: priority.id, className: "flex items-center gap-2" },
                h("span", { className: "w-32 text-sm" }, priority.name),
                h(
                  Select,
                  {
                    value: priorityMap[priority.id] || unmappedValue,
                    onValueChange: (value) => setPriorityMap({ ...priorityMap, [priority.id]: value === unmappedValue ? "" : value }),
                  },
                  h(SelectTrigger, { "data-testid": "redmine-priority-map-" + priority.id }, h(SelectValue, null)),
                  h(
                    SelectContent,
                    null,
                    [h(SelectItem, { key: unmappedValue, value: unmappedValue }, "Unmapped")].concat(["critical", "high", "medium", "low"].map((p) => h(SelectItem, { key: p, value: p }, p))),
                  ),
                ),
              ),
            ),
          ),
        ),
        h("div", { id: "redmine-custom-fields", "data-testid": "redmine-custom-fields" },
          h("h4", { className: "mb-2 text-sm font-medium" }, "Redmine custom fields"),
          (live.custom_fields || []).length ? h("ul", null, (live.custom_fields || []).map((field) => h("li", { key: field.id, "data-testid": "redmine-custom-field-" + field.id }, `#${field.id} ${field.name}`))) : h("p", { className: "text-muted-foreground text-xs" }, "No custom fields available."),
          live.custom_fields_derived ? h("p", { className: "text-muted-foreground text-xs", id: "redmine-custom-fields-derived-note" }, "Custom fields were derived from recent issues because this API key cannot list them.") : null,
        ),
      ),
      h(CardFooter, null, h(Button, { id: "redmine-fieldmapping-save", "data-testid": "redmine-fieldmapping-save", disabled: saving, onClick: onSave }, saving ? "Saving…" : "Save mapping")),
    );
  }

  // -- Sync options --------------------------------------------------------
  function SyncOptionsSection({ workspaceId, connected }) {
    const React = host.React;
    const [autoStatusWriteback, setAutoStatusWriteback] = React.useState(false);
    const [syncTitleDescription, setSyncTitleDescription] = React.useState(false);
    const [loading, setLoading] = React.useState(true);
    const [saving, setSaving] = React.useState(false);
    const controllerRef = React.useRef(null);
    const controller = syncControllerForWorkspace(
        controllerRef,
        invoke,
        workspaceId,
        toast,
        (next) => {
          setAutoStatusWriteback(next.autoStatusWriteback);
          setSyncTitleDescription(next.syncTitleDescription);
        },
        setSaving,
      );

    React.useEffect(() => {
      if (!connected) { setLoading(false); return; }
      let active = true;
      invoke("syncoptions.get", workspaceId, {}).then((options) => {
        if (!active) return;
        const next = { autoStatusWriteback: Boolean(options.auto_status_writeback), syncTitleDescription: Boolean(options.sync_title_description) };
        controller.setOptions(next);
        setAutoStatusWriteback(next.autoStatusWriteback);
        setSyncTitleDescription(next.syncTitleDescription);
      }).catch((err) => toast.error(errorMessage(err))).finally(() => { if (active) setLoading(false); });
      return () => { active = false; };
    }, [workspaceId, connected]);

    if (!connected) return null;
    if (loading) return h(Spinner, { id: "redmine-syncoptions-loading", "data-testid": "redmine-syncoptions-loading" });

    return h(
      Card,
      { id: "redmine-syncoptions-card" },
      h(CardHeader, null, h(CardTitle, null, "Sync options"), h(CardDescription, null, "Both default off: manual write-back only.")),
      h(
        CardContent,
        { className: "space-y-4" },
        h(
          "div",
          { className: "flex items-center justify-between" },
          h("div", null, h(Label, { htmlFor: "redmine-auto-writeback" }, "Automatic status write-back"), h(
            "p",
            { className: "text-muted-foreground text-xs" },
            "Push a mapped status to Redmine when a linked task moves to that workflow step.",
          )),
          h(Switch, {
            id: "redmine-auto-writeback",
            "data-testid": "redmine-auto-writeback",
            checked: autoStatusWriteback,
            disabled: saving,
            onCheckedChange: (checked) => {
              void controller.update("autoStatusWriteback", checked);
            },
          }),
        ),
        h(
          "div",
          { className: "flex items-center justify-between" },
          h("div", null, h(Label, { htmlFor: "redmine-sync-title" }, "Sync title & description"), h(
            "p",
            { className: "text-muted-foreground text-xs" },
            "Copy the Redmine subject/description onto the linked task on inbound sync.",
          )),
          h(Switch, {
            id: "redmine-sync-title",
            "data-testid": "redmine-sync-title",
            checked: syncTitleDescription,
            disabled: saving,
            onCheckedChange: (checked) => {
              void controller.update("syncTitleDescription", checked);
            },
          }),
        ),
      ),
    );
  }

  // -- Watchers --------------------------------------------------------
  function ReplyDraftingSection({ workspaceId, connected }) {
    const React = host.React;
    const currentWorkspace = React.useRef(workspaceId);
    currentWorkspace.current = workspaceId;
    const [settings, setSettings] = React.useState({ workflow_id: "", step_id: "", prompt_name: "client-reply" });
    const [workflows, setWorkflows] = React.useState([]);
    const [loading, setLoading] = React.useState(true);
    const [saving, setSaving] = React.useState(false);
    const [error, setError] = React.useState(null);
    React.useEffect(() => {
      let active = true;
      setLoading(true); setSaving(false); setError(null);
      if (!connected) { setLoading(false); return () => { active = false; }; }
      Promise.all([invoke("replies.settings.get", workspaceId, {}), invoke("workflows.list", workspaceId, {})]).then(([saved, options]) => {
        if (active) { setSettings({ ...saved, prompt_name: saved.prompt_name || "client-reply" }); setWorkflows(options.workflows || []); }
      }).catch(err => { if (active) setError(errorMessage(err)); }).finally(() => { if (active) setLoading(false); });
      return () => { active = false; };
    }, [workspaceId, connected]);
    const save = async () => {
      setSaving(true); setError(null);
      try { const saved = await invoke("replies.settings.save", workspaceId, settings); if (currentWorkspace.current === workspaceId) { setSettings(saved); toast.success("Reply drafting saved. Publishing still requires human approval."); } }
      catch (err) { if (currentWorkspace.current === workspaceId) setError(errorMessage(err)); }
      finally { if (currentWorkspace.current === workspaceId) setSaving(false); }
    };
    const steps = workflows.find(workflow => workflow.id === settings.workflow_id)?.steps || [];
    return h(Card, { id: "redmine-reply-settings" },
      h(CardHeader, null, h(CardTitle, null, "Client replies"), h(CardDescription, null, "Entering this step sends live public issue history to its read-only agent. The result is a draft; only an operator can approve and publish it.")),
      h(CardContent, { className: "space-y-3" },
        h(Label, { htmlFor: "redmine-reply-workflow" }, "Drafting workflow"),
        h(Select, { disabled: !connected || loading || saving, value: settings.workflow_id || "__none__", onValueChange: value => setSettings({ ...settings, workflow_id: value === "__none__" ? "" : value, step_id: "" }) },
          h(SelectTrigger, { id: "redmine-reply-workflow" }, h(SelectValue, { placeholder: "Choose workflow" })),
          h(SelectContent, null, h(SelectItem, { value: "__none__" }, "Disabled"), workflows.map(workflow => h(SelectItem, { key: workflow.id, value: workflow.id }, workflow.name)))),
        h(Label, { htmlFor: "redmine-reply-step" }, "Communication step"),
        h(Select, { disabled: !connected || loading || saving || !settings.workflow_id, value: settings.step_id || "__none__", onValueChange: value => setSettings({ ...settings, step_id: value === "__none__" ? "" : value }) },
          h(SelectTrigger, { id: "redmine-reply-step" }, h(SelectValue, { placeholder: "Choose step" })),
          h(SelectContent, null, h(SelectItem, { value: "__none__" }, "Disabled"), steps.map(step => h(SelectItem, { key: step.id, value: step.id }, step.name)))),
        h(Label, { htmlFor: "redmine-reply-prompt" }, "Saved prompt name"),
        h(Input, { id: "redmine-reply-prompt", value: settings.prompt_name, disabled: !connected || loading || saving, onChange: event => setSettings({ ...settings, prompt_name: event.target.value }) }),
        h("p", { className: "text-sm text-muted-foreground" }, "Use a fixed read-only profile, disable native auto-start and automatic transitions, and leave this step unmapped from Redmine status write-back. The plugin owns the single draft request. Empty step disables drafting. Automatic publishing is not enabled."),
        error ? h("p", { role: "alert" }, error) : null),
      h(CardFooter, null, h(Button, { disabled: !connected || loading || saving, onClick: save, "data-testid": "redmine-reply-save" }, "Save reply drafting")));
  }

  function WatchersSection({ workspaceId, connected }) {
    const React = host.React;
    const [watches, setWatches] = React.useState([]);
    const [loading, setLoading] = React.useState(true);
    const [projects, setProjects] = React.useState([]);
    const [filterOptions, setFilterOptions] = React.useState({ filters: [] });
    const [newProjectId, setNewProjectId] = React.useState("");
    const [activeFilters, setActiveFilters] = React.useState([]);
    const [filterToAdd, setFilterToAdd] = React.useState("");
    const [refreshingFilters, setRefreshingFilters] = React.useState(false);
    const [newMaxInflight, setNewMaxInflight] = React.useState("");
    const [creating, setCreating] = React.useState(false);
    const [busyWatchIDs, setBusyWatchIDs] = React.useState(new Set());

    const load = React.useCallback(async () => {
      if (!connected) {
        setLoading(false);
        return;
      }
      setLoading(true);
      try {
        const [result, projectResult] = await Promise.all([
          invoke("watches.list", workspaceId, {}), invoke("projects.list", workspaceId, {}),
        ]);
        setWatches((result && result.watches) || []);
        const selected = new Set((projectResult && projectResult.selected_ids) || []);
        setProjects(((projectResult && projectResult.projects) || []).filter((project) => selected.has(project.id)));
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setLoading(false);
      }
    }, [workspaceId, connected]);

    React.useEffect(() => {
      load();
    }, [load]);

    const refreshFilterOptions = async (projectID = newProjectId) => {
      const id = Number(projectID);
      if (!Number.isSafeInteger(id) || id <= 0) return;
      setRefreshingFilters(true);
      try {
        const options = await invoke("watches.filter_options", workspaceId, { project_id: id });
        setFilterOptions(options || { filters: [] });
      } catch (err) {
        toast.error(errorMessage(err));
      } finally { setRefreshingFilters(false); }
    };

    const selectProject = (value) => {
      const projectID = value === "__select_project__" ? "" : value;
      setNewProjectId(projectID);
      setActiveFilters([]); setFilterToAdd("");
      setFilterOptions({ filters: [] });
      if (projectID) void refreshFilterOptions(projectID);
    };

    if (!connected) return null;
    if (loading) return h(Spinner, { id: "redmine-watchers-loading" });

    const onCreate = async () => {
      if (creating) return;
      const projectId = Number(newProjectId);
      if (!Number.isSafeInteger(projectId) || projectId <= 0) {
        toast.error("Select a project.");
        return;
      }
      if (activeFilters.some((filter) => !filter.value)) { toast.error("Choose a value for every added filter."); return; }
      const maxInflight = Number(newMaxInflight);
      if (newMaxInflight !== "" && (!Number.isSafeInteger(maxInflight) || maxInflight < 0)) {
        toast.error("Max inflight tasks must be a non-negative integer.");
        return;
      }
      setCreating(true);
      try {
        await invoke("watches.create", workspaceId, {
          project_id: projectId,
          filters: activeFilters,
          max_inflight_tasks: newMaxInflight === "" ? 0 : maxInflight,
          enabled: true,
        });
        setNewProjectId("");
        setActiveFilters([]); setFilterToAdd("");
        setNewMaxInflight("");
        toast.success("Watch created.");
        await load();
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setCreating(false);
      }
    };

    const onToggle = async (watch) => {
      if (busyWatchIDs.has(watch.id)) return;
      setBusyWatchIDs((ids) => new Set(ids).add(watch.id));
      try {
        await invoke("watches.update", workspaceId, { ...watch, enabled: !watch.enabled });
        await load();
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setBusyWatchIDs((ids) => { const next = new Set(ids); next.delete(watch.id); return next; });
      }
    };

    const onDelete = async (watch) => {
      if (busyWatchIDs.has(watch.id)) return;
      setBusyWatchIDs((ids) => new Set(ids).add(watch.id));
      try {
        await invoke("watches.delete", workspaceId, { id: watch.id });
        toast.success("Watch removed.");
        await load();
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setBusyWatchIDs((ids) => { const next = new Set(ids); next.delete(watch.id); return next; });
      }
    };

    return h(
      Card,
      { id: "redmine-watchers-card" },
      h(
        CardHeader,
        null,
        h(CardTitle, null, "Issue watchers"),
        h(CardDescription, null, "Create one Kandev task per newly matching Redmine issue."),
      ),
      h(
        CardContent,
        { className: "space-y-4" },
        watches.length === 0
          ? h(Empty, { id: "redmine-watchers-empty" }, h(EmptyHeader, null, h(EmptyTitle, null, "No watches yet"), h(EmptyDescription, null, "Add one below.")))
          : h(
              Table,
              { id: "redmine-watchers-table" },
              h(
                TableHeader,
                null,
                h(TableRow, null, h(TableHead, null, "Project"), h(TableHead, null, "Max inflight"), h(TableHead, null, "Enabled"), h(TableHead, null, "")),
              ),
              h(
                TableBody,
                null,
                watches.map((watch) =>
                  h(
                    TableRow,
                    { key: watch.id },
                    h(TableCell, null, (projects.find((project) => project.id === watch.project_id) || {}).name || watch.project_id),
                    h(TableCell, null, watch.max_inflight_tasks || "unlimited"),
                    h(TableCell, null, h(Switch, { checked: watch.enabled, disabled: busyWatchIDs.has(watch.id), onCheckedChange: () => onToggle(watch) })),
                    h(TableCell, null, h(Button, { variant: "outline", size: "sm", disabled: busyWatchIDs.has(watch.id), onClick: () => onDelete(watch) }, "Delete")),
                  ),
                ),
              ),
            ),
        h(
          "div",
          { className: "grid gap-3 sm:grid-cols-2 lg:grid-cols-[1.2fr_1fr_auto] lg:items-end" },
          h(
            "div", { className: "space-y-2" },
            h(Label, { htmlFor: "redmine-new-watch-project" }, "Project"),
            h(Select, { value: newProjectId || "__select_project__", onValueChange: selectProject },
              h(SelectTrigger, { id: "redmine-new-watch-project", "data-testid": "redmine-watch-project", className: "w-full" }, h(SelectValue, { placeholder: "Select project" })),
              h(SelectContent, null, [h(SelectItem, { key: "__select_project__", value: "__select_project__" }, "Select project")].concat(projects.map((project) => h(SelectItem, { key: project.id, value: String(project.id) }, project.name)))),
            ),
          ),
          h(
            "div",
            { className: "space-y-2" },
            h(Label, { htmlFor: "redmine-new-watch-max" }, "Max inflight tasks"),
            h(Input, {
              id: "redmine-new-watch-max",
              "data-testid": "redmine-watch-max-inflight",
              type: "number",
              min: 0,
              placeholder: "0 = unlimited",
              value: newMaxInflight,
              onChange: (e) => setNewMaxInflight(e.target.value),
            }),
          ),
          h(Button, { id: "redmine-watchers-create", "data-testid": "redmine-watch-create", className: "w-full lg:w-auto", disabled: creating, onClick: onCreate }, creating ? "Creating…" : "Add watch"),
        ),
        newProjectId ? h(
          "div", { className: "space-y-3 rounded-md border p-3", "data-testid": "redmine-watch-filters" },
          h("div", { className: "flex items-center justify-between gap-2" }, h("div", null, h("h4", { className: "font-medium" }, "Redmine filters"), h("p", { className: "text-muted-foreground text-sm" }, "Add the live Redmine fields you want to match. Choices are scoped to this project.")), h(Button, { type: "button", variant: "outline", size: "sm", "data-testid": "redmine-watch-filters-refresh", disabled: refreshingFilters, onClick: () => { void refreshFilterOptions(); } }, refreshingFilters ? "Refreshing…" : "Refresh")),
          h("div", { className: "max-w-md space-y-2" },
            h(Label, { htmlFor: "redmine-watch-filter-add" }, "Add filter"),
            h(Select, { value: filterToAdd || "__add_filter__", onValueChange: (value) => { if (value !== "__add_filter__") { const option = (filterOptions.filters || []).find((candidate) => candidate.field === value); if (option) { setActiveFilters(activeFilters.concat({ field: option.field, operator: (option.operators || [])[0]?.value || "=", value: "" })); setFilterToAdd(""); } } } },
              h(SelectTrigger, { id: "redmine-watch-filter-add", "data-testid": "redmine-watch-filter-add" }, h(SelectValue, { placeholder: "Add Redmine filter" })),
              h(SelectContent, null, [h(SelectItem, { key: "__add_filter__", value: "__add_filter__" }, "Add Redmine filter")].concat((filterOptions.filters || []).filter((option) => !activeFilters.some((filter) => filter.field === option.field)).map((option) => h(SelectItem, { key: option.field, value: option.field }, option.name)))),
            ),
          ),
          activeFilters.map((filter) => {
            const option = (filterOptions.filters || []).find((candidate) => candidate.field === filter.field);
            if (!option) return null;
            const change = (changes) => setActiveFilters(activeFilters.map((candidate) => candidate.field === filter.field ? { ...candidate, ...changes } : candidate));
            const remove = () => setActiveFilters(activeFilters.filter((candidate) => candidate.field !== filter.field));
            const valueControl = option.kind === "select"
              ? h(Select, { value: filter.value || "__filter_value__", onValueChange: (value) => change({ value: value === "__filter_value__" ? "" : value }) }, h(SelectTrigger, { "data-testid": `redmine-watch-filter-value-${filter.field}` }, h(SelectValue, { placeholder: "Select value" })), h(SelectContent, null, [h(SelectItem, { key: "__filter_value__", value: "__filter_value__" }, "Select value")].concat((option.values || []).map((value) => h(SelectItem, { key: value.value, value: value.value }, value.name)))))
              : h(Input, { "data-testid": `redmine-watch-filter-value-${filter.field}`, type: option.kind === "date" ? "date" : option.kind === "number" ? "number" : "text", value: filter.value, placeholder: `Enter ${option.name.toLowerCase()}`, onChange: (event) => change({ value: event.target.value }) });
            return h("div", { key: filter.field, className: "grid gap-2 rounded border p-2 sm:grid-cols-4 sm:items-end", "data-testid": `redmine-watch-filter-${filter.field}` },
              h("div", null, h(Label, null, "Filter"), h("p", { className: "pt-2 text-sm" }, option.name)),
              h("div", { className: "space-y-1" }, h(Label, null, "Operator"), h(Select, { value: filter.operator, onValueChange: (value) => change({ operator: value }) }, h(SelectTrigger, { "data-testid": `redmine-watch-filter-operator-${filter.field}` }, h(SelectValue, null)), h(SelectContent, null, (option.operators || []).map((operator) => h(SelectItem, { key: operator.value, value: operator.value }, operator.name))))),
              h("div", { className: "space-y-1" }, h(Label, null, "Value"), valueControl),
              h(Button, { type: "button", variant: "ghost", size: "sm", "data-testid": `redmine-watch-filter-remove-${filter.field}`, onClick: remove }, "Remove"),
            );
          }),
        ) : null,
      ),
    );
  }

  // -- Page ----------------------------------------------------------
  return function RedmineSettingsPage({ workspaceId }) {
    const React = host.React;
    const [connection, setConnection] = React.useState(null);
    const [loading, setLoading] = React.useState(true);

    const reload = React.useCallback(async () => {
      if (!workspaceId) {
        setLoading(false);
        return;
      }
      setLoading(true);
      try {
        const result = await invoke("connection.get", workspaceId, {});
        setConnection(result);
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setLoading(false);
      }
    }, [workspaceId]);

    React.useEffect(() => {
      reload();
    }, [reload]);

    if (!workspaceId) {
      return h(
        Empty,
        { id: "redmine-settings-no-workspace" },
        h(EmptyHeader, null, h(EmptyTitle, null, "Select a workspace"), h(EmptyDescription, null, "Redmine connections are per-workspace.")),
      );
    }
    if (loading || !connection) return h(Spinner, { id: "redmine-settings-loading" });

    const connected = connection.state === "connected" || connection.state === "degraded";

    return h(
      "div",
      { className: "space-y-4 p-4 max-w-3xl" },
      h(ConnectionSection, { workspaceId, connection, reload }),
      h(ProjectsSection, { workspaceId, connected }),
      h(FieldMappingSection, { workspaceId, connected }),
      h(SyncOptionsSection, { workspaceId, connected }),
      h(ReplyDraftingSection, { workspaceId, connected }),
      h(WatchersSection, { workspaceId, connected }),
    );
  };
}

// ---------------------------------------------------------------------------
// Registration. Keep only what this plugin uses.
// ---------------------------------------------------------------------------
window.registerKandevPlugin("kandev-plugin-redmine", {
  initialize(registry, host) {
    const redmineIcon = makeRedmineIcon(host);
    registry.registerTaskAction(makeLinkTaskAction(host));
    registry.registerTaskMenuAction(makeSetRedmineStatusAction(host));
    registry.registerTaskMenuAction(makeRedmineCommentAction(host));
    registry.registerTaskMenuAction(makeUnlinkRedmineAction(host));
    registry.registerIntegrationSettings({
      id: "redmine",
      label: "Redmine",
      description: "Link tasks to Redmine issues, sync status both ways, and watch for new issues.",
      icon: redmineIcon,
      Component: makeSettingsComponent(host),
      action: makeIntegrationEnabledAction(host),
    });
  },

  destroy() {
    // The host bulk-unregisters everything under this plugin's id; no
    // module-level state to reset (every section's state lives in React
    // component state, torn down with the component itself).
  },
});
