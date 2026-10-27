# Workflow routing and automatic intake by Redmine tracker

In **Field mapping → Tracker → new task workflow**, select a live Redmine
tracker, a workspace workflow and its entry step. Other trackers keep the
**Default workflow** and its status map. Tracker names, users, status IDs,
projects and workflow IDs are configuration; none is compiled into the plugin.

## Entry and status mapping

Each tracker override has its own **Statuses → workflow step** rows. The same
Redmine status may point to different steps for different trackers. An override
never falls back to the default status map; an unmapped status leaves its task
stage unchanged. Title, description and priority synchronization remain available.

For routed workflows, inbound moves respond to actual Redmine status or tracker
changes. Repeated observations of an unchanged working status do not reset
internal progress back to its entry step. Communication approval steps remain held.
Changing a route's workflow/entry affects future imports, not existing tasks;
status-map changes apply within tasks' existing workflows. Watch IDs, links,
deduplication records and in-flight work are preserved.

An entry step may explicitly enable native `auto_start_agent`. The plugin asks
Kandev to start an agent when importing into that configured route step. Without
it, imports remain manual. For the default workflow, only the native start step
may auto-start; an old status-filter watch pointing to an intermediate stage
does not gain automatic execution after upgrade.

## Automatic workflow intake without global write-back

Enable **Automatically start workflow for issues assigned to the token user** on the
selected tracker route. Choose a source status (for example New) and a distinct
open working status (for example In Progress). Both choices come from live
Redmine data. Keep global **Automatic status write-back** off when no other
automatic status updates are wanted.

The rule imports only watch-matching issues of this tracker that are in the
source status and assigned to the user returned by `/users/current.json` for
the current connection token. Other issues remain unseen and can qualify on a
later poll after assignment. It does not reassign tickets, replay existing
imports or publish a customer reply. The selected native workflow determines
what happens after entry; the plugin does not assume a diagnosis-only process.

Configure the route entry with a fixed native profile and explicit auto-start.
Source and working statuses must stay unmapped or point to that entry, so the
claim does not inadvertently select a different stage. Diagnosis-only workflows
can use read-only profiles; implementation workflows can use their authorized
write profiles. The plugin preserves native permissions and MCP access. The
workflow owner controls stage transitions and any required human gates.

Every automatic start requires a **positive Max inflight tasks** limit. The
server also caps launch attempts within each poll to this limit, including
rejected launches. Zero/unlimited remains available for manual imports but
cannot start agents. A configured entry with no fixed profile also fails closed.
Use **Edit execution** on an existing watch to set this limit without deleting
its tasks, links, filters or deduplication records.

For code work, select an existing workspace **Code repository** on the watch.
An empty base branch uses the repository's native default; an explicit branch is
forwarded unchanged to Kandev. The existing Git integration, credentials and
workspace policies handle checkout and worktrees. The plugin exposes only
repository IDs, names and default branches through the new read-only
`repositories` capability; it does not fetch credentials or add a Git provider.
Existing imported tasks are not retroactively attached or migrated.

For example, one tracker can enter a read-only Diagnosis step and stop at a
communication approval. Another can enter Plan and proceed through
implementation, independent review, dev deployment and browser acceptance before
a manual handoff. These names, profiles and transitions are examples, not built-in
tracker semantics. MR creation, pushes, dev reservation, screenshots and the
manual handoff belong to the selected workflow's explicit instructions, not to
the intake rule.

On qualified automatic intake the host-owned plugin task metadata records the
selected workflow/entry/profile, token-owner ID, status transition and selected
repository ID under `plugin:kandev-plugin-redmine.automatic_intake`. The task
body cannot grant these permissions. Workflow instructions may validate this
provenance together with the actual attached repository before executing the
operator-authorized delivery flow; no task text alone grants publishing rights.

Kandev task creation is best-effort for agent launch. On a subsequent poll, the
plugin checks the original task/link/watch/workflow, the unchanged rule and token
owner, and the first session's configured profile and ACP session ID. Created,
starting, failed or unrelated sessions do not qualify. After a confirmed launch,
it reads the issue again and sends a **status-only** update if tracker, project,
source status and assignee still match. A final read confirms the working status.
It stores a receipt and suppresses the inbound echo; repeated polls do not claim
the same import again. This verifies launch, not completion or workflow outcome.

Redmine's standard REST issue response exposes no lock version. The fresh read
and subsequent PUT are not atomic with a simultaneous human edit. Strict atomic
takeover would require a conditional endpoint on the Redmine server.

An uncertain PUT is never automatically repeated. Later polls only read the
issue to reconcile it; a mismatch requires operator inspection. An uncertain
auto-start task creation is also held instead of retried or compensated by
deleting potentially running work. Its `autostart_issue.<issue-id>` workspace-state
record identifies the known native task when available, occupies an inflight
slot, and blocks watch/connection removal until inspected. Errors include the
issue/task reference; independent watches and available slots still progress.
This version has no automatic recovery or override for an uncertain launch.

## Communication and deployment gates

A bug workflow can use diagnosis → initial client communication → repair plan →
implementation → review → dev deployment → browser acceptance → fix-ready
communication → done. Add both communication steps in **Client replies**, with
different saved prompts and automatic publishing off. Leave native auto-start
and automatic exit disabled on both, and keep them unmapped from Redmine
statuses. A human reviews the exact reply and owns the transition.

Shared dev reservations, authorized deployers, schema and rollback checks,
mail capture, test evidence and screenshots remain workflow policies. This
feature grants no production deployment or automatic
Redmine closure at Done. A fix-ready reply must distinguish dev acceptance from
production deployment and must not invent a release date.
