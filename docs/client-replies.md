# Client replies with configurable publication

Reply generation and publication are separate. Existing connections, API keys,
selected projects, mappings and watchers are reused; instance/project/user names
and writing rules are never compiled into the plugin. The plugin ID is unchanged.

## Configure

1. Grant workspace-scoped message read/write and session/interaction read access
   in addition to existing task/workflow access. No Redmine credential is copied
   to the agent, and the plugin never answers pending human interactions.
2. Create a saved Kandev prompt for the client's language, tone and evidence rules.
3. Add a communication step with a fixed read-only agent profile. Disable native
   auto-start and all automatic exit transitions: the plugin owns the initial
   message, and a human owns the exit. Do not map this step to a Redmine status.
4. In the Redmine integration's **Client replies** section, select the workflow,
   step and exact saved prompt name. Use **Add communication step** for additional
   stages or workflows, for example diagnosis and fix-ready messages. Each entry
   has its own prompt and publication mode. An empty default step disables that
   entry; remove unused additional entries. All configured communication steps
   are held against inbound status-sync moves; ordinary field sync continues.
5. Route the desired task handoff to this step. Keep the connection enabled.

**Publish replies automatically** is off by default, including upgrades and
workspaces with existing settings. Off means an operator must approve each exact
comment. Turning it on and saving authorizes the plugin to append completed
replies without per-comment approval. The setting is independent of the empty-step
drafting switch and the integration's overall enabled/paused switch.
The original single-step configuration is preserved on upgrade. New entries
default to manual approval. Automatic publishing consent is scoped to one
workflow, step and prompt; moving to another stage cannot publish the previous
stage's draft. Saving another entry does not rotate unrelated publication consent.

Entering the step sends one draft request with live issue details and public
journals to the newest live session of its configured read-only profile. The
session ID is pinned explicitly; the task's default session may still belong to
an earlier implementation step and is never used implicitly. A task with no
matching session is rejected before recording a dispatch: open it in Kandev,
initialize a session of the communication profile, then select **Generate from
current issue**. Competing active sessions and incomplete session history also
block new dispatches. Existing uncertain attempts remain held for native
inspection; the upgrade does not replay or clear them.

Private journals are omitted. Issue text is data,
not authority. Repeated events and clicks coalesce while a request is active.
An unconfirmed dispatch is retained for native inspection, never blindly replayed.
Paused connections, wrong steps, changed links and queued workflow admissions do
not start new draft work. After admission or native recovery, an operator can use
**Generate from current issue** to make an explicit request.
After a verified completed response, an operator may request a new draft even if
the previous response reported missing evidence or contained no valid proposal.
Completion is checked in that request's originating session, so intervening
implementation or test-session messages do not block a later communication stage.
An unstarted session for the next stage can receive its new request. Automatic
publication retains the stricter check of the latest response across the task and
requires all other sessions to be inactive.
Running agents, pending human input and unconfirmed dispatches still block another
request; replaying the same request ID never starts new work.

The drafting agent keeps evidence and validation limits in the task plan. Its
final message contains only:

```redmine-comment
{"issue_id":42,"request_id":"the exact request ID supplied in the prompt","private_notes":false,"notes":"The proposed client reply."}
```

The example ID is illustrative; every real draft must match its live task link.
There is no approved flag. Missing/unsafe evidence should produce a Kandev blocker,
not a publishable draft. Writing prompts do not control publication permissions.

## Approve and publish

Open **Review Redmine reply** from the task menu. The latest complete structured
agent message can prefill the editor; you can also enter a proposal manually.
When it differs from the last published reply, the new proposal opens in the
editor and the previous publication stays in a separate read-only disclosure.
**New comment** refreshes the latest proposal and does not prefill an already
published response again.
Review the linked issue, edit the text, and explicitly select public or private
visibility. **Review exact comment** stores an immutable preview for this operator,
task, issue, writer identity and issue-context fingerprint. It performs no Redmine
write. Editing discards that preview; a replacement gets a new ID.

With automatic publication off, only **Approve and publish public comment/private
note** submits the stored preview. The request contains its ID, not replacement
text or issue fields.
The backend rechecks the actor, link, selected project, connection revision,
publishing identity and current issue context. It appends only `notes` and
`private_notes`; status, assignment, title and billing stay unchanged.

The append has no transport retry. Readback must find one matching new journal
with the expected author, content and visibility. Replay of an accepted draft
returns its receipt. An uncertain attempt blocks replacement and offers
**Check publication result**, which reads only. If it stays ambiguous, inspect the
native task and Redmine; do not delete plugin state or resend blindly. Definitive
authorization/validation rejection allows a newly reviewed attempt after repair.

Previews and publication receipts retain the operator and time in task-scoped
plugin state. The native task and Redmine journal retain their corresponding
history. Approval never marks the Kandev task done; the operator checks the scoped
technical acceptance and moves the task when appropriate.

## Automatic publication

Enabling the switch creates a server-generated policy identity attributed to the
authenticated operator. Only requests started under that exact active policy can
publish automatically. Enabling it does not send an existing draft or preview.
Disabling and re-enabling, or changing the workflow/step/prompt, invalidates old
automatic requests. A comment already being submitted cannot be recalled.

The existing connector poll loop checks completed responses, normally once per
minute. It verifies the linked issue and connection are unchanged since dispatch,
the task remains in the communication step, the expected session/profile is idle
or complete, no other session is running, and no human interaction is pending.
The latest final agent message must match the request ID, session, issue and
timestamp, with a nonempty turn ID; incomplete or unrelated results cannot publish. Private
notes require the explicit visibility field just like public comments.

The plugin stores the exact response and its source request/message, then uses
the same single-attempt publisher and journal readback as manual approval. Its
receipt records `automatic-policy:<id>` rather than inventing human approval of
that response. Restarts and polling never replay an attempted append. Failed or
stale evidence, configuration changes and manual previews hold the request for
review; the task menu shows the reason. Pending questions and running agents wait.
Manual publication remains available after preparing a new human-review preview.

Automatic publication does not change issue fields or complete/move the task.
The agent still drafts without a Redmine credential; the plugin alone publishes.

## Boundaries

- Agent permission auto-approval is unrelated to this switch. Drafting stays
  read-only in both modes; only the plugin's authenticated workspace setting can
  authorize automatic publication.
- Draft discovery is bounded to 1,000 task messages. A malformed cursor, missing
  message grant, oversized live context (128 KiB) or absent valid final draft is
  shown for manual attention rather than silently using an older proposal.
- Automatic discovery scans at most 2,000 workflow tasks and 100 sessions per
  task. Missing read grants, incomplete session history or uncertain completion
  cannot authorize publication.
- The workflow SDK exposes profile IDs and entry actions, not profile permission
  modes or exit transitions. The operator must verify read-only mode and the
  manual exit in Kandev; the settings handler rejects native auto-start and a
  status-mapped communication step.
- Redmine offers no append idempotency key or atomic compare-and-append here.
  Context is checked immediately before submission. Matching journal readback
  confirms stored content; it cannot uniquely attribute an identical concurrent
  append by another client sharing the same account. Ambiguous matches stay open.
- Local fixture/contract tests do not prove a live provider run, real Redmine
  delivery, visual layout or client acceptance. Test one disposable linked issue
  after installation before connecting the normal delivery route.

CI **Build** runs upload an installable five-platform archive and `SHA256SUMS`
under the `kandev-plugin-redmine` artifact. Upload the archive to the existing
installation; do not uninstall the connector or change its ID to upgrade.
