# Workflow routing and diagnosis by Redmine tracker

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
internal progress back to diagnosis. Communication approval steps remain held.
Changing a route's workflow/entry affects future imports, not existing tasks;
status-map changes apply within tasks' existing workflows. Watch IDs, links,
deduplication records and in-flight work are preserved.

An entry step may explicitly enable native `auto_start_agent`. The plugin asks
Kandev to start an agent when importing into that configured route step. Without
it, imports remain manual. For the default workflow, only the native start step
may auto-start; an old status-filter watch pointing to an intermediate stage
does not gain automatic execution after upgrade.

## Diagnostic takeover without global write-back

Enable **Automatically diagnose issues assigned to the token user** on the
selected tracker route. Choose a source status (for example New) and a distinct
open working status (for example In Progress). Both choices come from live
Redmine data. Keep global **Automatic status write-back** off when no other
automatic status updates are wanted.

The rule imports only watch-matching issues of this tracker that are in the
source status and assigned to the user returned by `/users/current.json` for
the current connection token. Other issues remain unseen and can qualify on a
later poll after assignment. It does not reassign tickets, replay existing
imports, start implementation, or publish a customer reply.

Configure the route entry with a fixed **read-only** native profile and explicit
native auto-start. Source and working statuses must stay unmapped or point to
that diagnostic entry, so the claim cannot select a later implementation stage.
The plugin does not change profile permissions or MCP access, nor can this SDK
verify the profile's permission mode through the already granted capabilities.
The workflow owner must keep the diagnostic profile read-only and its next step
human-controlled.

Kandev task creation is best-effort for agent launch. On a subsequent poll, the
plugin checks the original task/link/watch/workflow, the unchanged rule and token
owner, and the first session's configured profile and ACP session ID. Created,
starting, failed or unrelated sessions do not qualify. After a confirmed launch,
it reads the issue again and sends a **status-only** update if tracker, project,
source status and assignee still match. A final read confirms the working status.
It stores a receipt and suppresses the inbound echo; repeated polls do not claim
the same import again. This verifies launch, not completion or diagnostic quality.

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
feature grants no new host capabilities, production deployment or automatic
Redmine closure at Done. A fix-ready reply must distinguish dev acceptance from
production deployment and must not invent a release date.
