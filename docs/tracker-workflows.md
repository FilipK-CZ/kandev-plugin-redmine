# Workflow routing by Redmine tracker

In **Field mapping → Tracker → new task workflow**, add a live Redmine tracker,
a workspace workflow and an explicit entry step, then save the mapping. Trackers,
projects and workflow IDs are configuration, never compiled into the plugin.

The route overrides the existing watch's default placement only when creating a
new watcher task. Other trackers keep the default workflow. Existing imports,
watch IDs, deduplication records, links and in-flight work are not migrated or
replayed. Removing a route affects future imports only.

Entry steps must not have native `auto_start_agent`; importing an issue is not
permission to execute it. An operator starts the diagnosis. The default Redmine
status mapping only moves tasks within its own workflow, so routed tasks cannot
be pulled back into the default workflow. Title, description and priority sync
remain available. Status write-back is unchanged and does not gain new mappings.

A bug workflow can use diagnosis → initial client communication → repair plan →
implementation → review → dev deployment → browser acceptance → fix-ready
communication → done. Add both communication steps in **Client replies**, with
different saved prompts and automatic publishing off. Leave native auto-start
and automatic exit transitions disabled on both; a human reviews the exact reply
and owns the transition. Keep any other workflow's communication entry.

Deployment, shared-environment reservations, test evidence, screenshots and
production authorization belong to the workflow's existing policies. This
feature does not authorize a production deployment or close a Redmine issue when
a task reaches Done. A fix-ready message must distinguish dev acceptance from
production deployment and must not invent a deployment date.
