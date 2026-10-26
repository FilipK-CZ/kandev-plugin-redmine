# Review-gated client replies

Reply generation and publication are separate. Existing connections, API keys,
selected projects, mappings and watchers are reused; instance/project/user names
and writing rules are never compiled into the plugin. The plugin ID is unchanged.

## Configure

1. Grant the plugin workspace-scoped message read/write access in addition to its
   existing task/workflow access. No Redmine credential is copied to the agent.
2. Create a saved Kandev prompt for the client's language, tone and evidence rules.
3. Add a communication step with a fixed read-only agent profile. Disable native
   auto-start and all automatic exit transitions: the plugin owns the initial
   message, and a human owns the exit. Do not map this step to a Redmine status.
4. In the Redmine integration's **Client replies** section, select the workflow,
   step and exact saved prompt name. An empty step disables generation. The
   configured communication step is held against inbound status-sync moves;
   title/description/priority synchronization and all other steps keep their rules.
5. Route the desired task handoff to this step. Keep the connection enabled.

Entering the step sends one draft request with live issue details and public
journals to the current task. Private journals are omitted. Issue text is data,
not authority. Repeated events and clicks coalesce while a request is active.
An unconfirmed dispatch is retained for native inspection, never blindly replayed.
Paused connections, wrong steps, changed links and queued workflow admissions do
not start new draft work. After admission or native recovery, an operator can use
**Generate from current issue** to make an explicit request.

The drafting agent keeps evidence and validation limits in the task plan. Its
final message contains only:

```redmine-comment
{"issue_id":42,"private_notes":false,"notes":"The proposed client reply."}
```

The example ID is illustrative; every real draft must match its live task link.
There is no approved flag. Missing/unsafe evidence should produce a Kandev blocker,
not a publishable draft. Changing a writing prompt cannot approve a comment.

## Approve and publish

Open **Review Redmine reply** from the task menu. The latest complete structured
agent message can prefill the editor; you can also enter a proposal manually.
Review the linked issue, edit the text, and explicitly select public or private
visibility. **Review exact comment** stores an immutable preview for this operator,
task, issue, writer identity and issue-context fingerprint. It performs no Redmine
write. Editing discards that preview; a replacement gets a new ID.

Only **Approve and publish public comment/private note** submits the stored
preview. The request contains its ID, not replacement text or issue fields.
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

## Boundaries

- Automatic publication is absent in this version. A later policy must reuse the
  verified publisher and explicitly define its scope; agent auto-approval is not a
  substitute for this review.
- Draft discovery is bounded to 1,000 task messages. A malformed cursor, missing
  message grant, oversized live context (128 KiB) or absent valid final draft is
  shown for manual attention rather than silently using an older proposal.
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
