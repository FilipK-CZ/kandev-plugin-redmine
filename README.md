# kandev-plugin-redmine

A Kandev **native-UI plugin** connecting a
Redmine instance to kandev: link tasks to Redmine issues, sync status both
ways, and watch for new issues. Bootstrapped from
`kandev-plugin-template`; the authoritative plugin specification and
verification trace is [docs/spec.md](docs/spec.md).

This fork adds [review-gated client replies](docs/client-replies.md) and
[workflow routing by tracker](docs/tracker-workflows.md). It keeps
the original plugin ID and connection/state format for an in-place upgrade.
Reply generation is disabled until a workflow step is configured. Publication
requires human approval by default; an explicit workspace switch enables
automatic publication of new, verified draft responses.

- **Connection** — API-key auth (`X-Redmine-API-Key`) only, one connection per
  kandev workspace, validated against `GET /users/current.json`. The key is
  passed as plaintext only to the SDK's host-managed encrypted secret store.
  The plugin uses a dot-safe composed key (`redmine.<workspace_id>.api_key`).
  Existing v0.1 plugin-encrypted values are dual-read and migrated on read;
  new values never use plugin-side encryption.
- **No Redmine administrator required** — a normal Redmine user API key is
  sufficient. Project and issue lists contain only resources that user can
  access. Issue creation, edits, and status write-back additionally require
  the corresponding project-role permissions. If the admin-only custom-field
  endpoint returns 403, fields are derived from issues visible to that user.
- **Workspace enable switch** — each Kandev workspace can pause Redmine
  polling and automatic write-back without deleting its connection, secret,
  mappings, or watcher definitions.
- **Own health polling** — no host `healthpoll` equivalent exists for plugins;
  this plugin runs its own ~90s jittered probe per connected workspace.
- **Project and field mapping** — statuses, trackers, and priorities are always
  fetched live from the instance. Users choose a workspace workflow, add only
  the Redmine statuses they want to map, and select a Kandev step for each;
  task priorities remain configurable and nothing is hardcoded. Trackers stay
  available for Redmine issue fields and watcher filters, but the Host task
  contract does not allow plugins to mutate Kandev labels.
- **Issue read/write** — always sends `status_id=*` (Redmine defaults to
  open-only), with attachments via the two-step upload-token flow.
- **Client replies** — configurable read-only communication steps receive fresh
  public issue history and drafts a reply using a saved Kandev prompt. The
  task menu's **Review Redmine reply** opens the proposal for editing and
  explicit approval. Comment publication changes only `notes` and
  `private_notes`, verifies the resulting journal, and never retries an
  uncertain append. A workspace switch can publish new verified drafts
  automatically; human approval remains the default. Normal issue-field updates
  keep their existing behavior.
- **Task linking** — `registerTaskAction({placement:"link"})` +
  `host.openTaskLinkDialog`, the same shared native Link surface GitHub/
  Bitbucket use.
- **Bidirectional sync** — cursor-based polling with opt-in outbound
  write-back (`autoStatusWriteback`, `syncTitleDescription`) and echo
  suppression so a write-back round trip never bounces the task. Each tracker
  route can have its own status map, independent of the default workflow.
- **Automatic workflow intake** — an opt-in tracker rule imports source-status issues
  assigned to the token's actual user, starts the configured entry agent and
  claims only the working status after launch is verified. Global status
  write-back and automatic customer replies can remain off. The chosen workflow
  defines the job: diagnosis, planning, implementation or another configured
  process. A fixed profile and a positive watch capacity are required. Watchers
  can attach an existing workspace repository and optional base branch; execution
  settings can be edited without recreating the watch or its tasks.
- **Composer `#` mentions** — resolves Redmine issues through
  `reference_sources` with submit-time reauthorization.
- **Issue watchers** — one kandev task per newly matching issue, deduplicated
  and throttled per watch (`maxInflightTasks`), cascade-deleted via
  `PluginOwnedTaskTrees` when the watch or connection is removed. Disconnect
  before uninstalling: host uninstall has no pre-uninstall task-tree hook.
- **Settings UI** — `registerIntegrationSettings` contributes a native
  connection form, project picker, field-mapping table, sync-option toggles,
  and watcher management, rendered inside the kandev SPA (not an iframe).

## Use the host's React — and the host's recharts

`initialize(registry, host)` hands you `host.React` (and `host.jsx`, an alias
for `host.React.createElement`). Use it. **Never import or bundle your own
React**: a second React instance has its own hook dispatcher and context
registry, so host components rendered inside your tree lose their providers,
refs break, and `asChild` stops composing.

The same hazard applies to **recharts**: use the `host.ui.Chart*` wrappers
instead of installing your own copy, for the same reason — a second instance
splits the React context recharts' tooltips and legends resolve through.

`host.ui` is broad — `Accordion*`, `Collapsible*`, `Select*`, `Tabs*`,
`Sheet*`, `Pagination*`, `ScrollArea`, `Skeleton`, `Switch`, `Spinner`,
`TooltipProvider`, plus kandev's own `PageTopbar`, `Combobox` and
`TaskCreateDialog`. Check it before hand-rolling a component. The
authoritative list is `PLUGIN_UI` in `apps/web/lib/plugins/host-api.ts` in
`kdlbs/kandev`.

## Actions vs. webhooks

This plugin's own settings UI calls its Go backend through **`actions:`**
entries in `manifest.yaml` (`host.api.invokeAction` on the frontend,
`pluginsdk.ActionHandler.HandleAction` on the backend) — the host
authenticates and authorizes every call and passes a verified
`VerifiedActionContext` (workspace/task ID) the backend must derive its
`plugin_state`/secret keys from, never a request-body-supplied one. There are
no `webhooks:` entries: Redmine core has no outbound webhooks, so nothing
calls into this plugin from the outside.

## Minimum host version

`min_kandev_version: "0.94.0"` — kdlbs/kandev v0.94.0 is the first
upstream release containing the generic task priority and
workflow-placement contract from kdlbs/kandev#2872 (merge commit
`5f5b1f0`, merged 2026-09-06). Verified against the kandev history:
`git merge-base --is-ancestor 5f5b1f0f319e688c7aa4f161c30e0dd024d3f1eb v0.94.0`
succeeds, while the same check against `v0.93.0` fails. The release
workflow's preflight (`scripts/require-released-kandev-sdk-contract.sh`)
revalidates that ancestry before any release mutation.

It is **release-only by design**: a host built from a git checkout reports a
git-describe version like `v0.87.1-27-g4705f1fd0`, which isn't a release
version, so the gate is skipped and the install succeeds whatever floor is
declared. A successful sideload onto a dev instance is not evidence the floor
is correct — check it against the kandev history instead
(`git merge-base --is-ancestor <commit> <tag>`).

## How a plugin runs (gRPC subprocess, not HTTP)

kandev spawns the platform-matching binary from `runtime.executables` in
`manifest.yaml` as a subprocess and talks to it over a private gRPC connection
([hashicorp/go-plugin](https://github.com/hashicorp/go-plugin)) — there is no
HTTP listen address, no shared secret, and no manual wiring: `pluginsdk.Serve`
in `server/main.go` owns the entire transport. The base interface has two RPCs
and gives you a `Host` handle back:

```go
type Plugin interface {
    OnEvent(ctx context.Context, e *Event) error
    HandleWebhook(ctx context.Context, req *WebhookRequest) (*WebhookResponse, error)
}
```

`server/plugin.go`'s plugin type embeds `pluginsdk.UnimplementedPlugin` (a
no-op default for both RPCs, plus `Host()`/`SetHost()` accessors). Newer
features are additive optional interfaces this plugin implements as needed:
`pluginsdk.ActionHandler` (settings UI backend calls),
`pluginsdk.EntityReferenceSearcher` + `EntityReferenceAuthorizer` (composer
`#` mentions), and `pluginsdk.PluginOwnedTaskTreeHost` consumption via
`Host.PluginOwnedTaskTrees()` (watcher task cascade delete).

## Developing against the SDK

`pkg/pluginsdk` is not published as its own module yet, so `go.mod` here uses
a local `replace`:

```
replace github.com/kandev/kandev => ../kandev/apps/backend
```

This assumes your plugin repo is checked out as a **sibling** of the `kandev`
monorepo:

```
some-dir/
├── kandev/                   # https://github.com/kdlbs/kandev, Go module at apps/backend/
└── kandev-plugin-redmine/    # this repo
```

Note the module root is `kandev/apps/backend`, not the repo root — `kandev` is
a monorepo and the Go backend (including `pkg/pluginsdk`) lives one level down.
Adjust the `replace` path if your layout differs. Once `pkg/pluginsdk` ships as
a standalone, versioned module, this repo will drop the `replace` and pin a
real version instead.

## Layout

```
manifest.yaml          # plugin manifest — id, capabilities, actions, reference_sources, runtime.executables, ui.bundle
server/
  main.go              # pluginsdk.Serve wiring — no flags, no HTTP, no secrets
  plugin.go            # the redmine plugin type: OnEvent / HandleWebhook / ActionHandler / EntityReferenceSearcher
  plugin_test.go       # tests against a fake Host, no subprocess spawn needed
  <package>/           # connection, projects, fieldmapping, issues, sync, tasklink, watch (per plan task)
ui/
  bundle.js            # hand-written, no-build ES module — the plugin's frontend half
```

`ui/bundle.js` is hand-written, dependency-free ES module JavaScript. There is
no build step: it ships byte-for-byte inside the package tar.gz, and kandev
serves it directly. Edit the file and repackage — nothing else to run.

## Build and test

```sh
make build               # go build -o bin/... ./server/...
make test                # go test ./... -race
make vet                 # go vet ./...
make test-ui             # native settings/task-action UI contract tests
make verify-package-host # validate a host-only tarball and checksums

# Packaged UI smoke test against a disposable Kandev host:
KANDEV_PLUGIN_E2E_URL=http://127.0.0.1:13081 make e2e

# Full live-Redmine acceptance run (API keys are write-only environment input):
KANDEV_PLUGIN_E2E_URL=http://127.0.0.1:13081 \
KANDEV_REDMINE_E2E_BASE_URL=http://127.0.0.1:13080 \
KANDEV_REDMINE_E2E_API_KEY='<first disposable key>' \
KANDEV_REDMINE_E2E_ROTATED_API_KEY='<second disposable key>' \
make e2e-live
```

> Note: bare `go build ./server/...` (no `-o`) fails with `build output
> "server" already exists and is a directory` — Go's default output name for a
> lone main package is the last path element ("server"), which collides with
> the `server/` source directory. Always pass `-o`, run `go build .` from
> inside `server/`, or use `make build`. `go vet`/`go test` are unaffected.

## Package it

```sh
make package        # cross-compiles linux/darwin (amd64+arm64) + windows/amd64,
                    # then packs manifest + ui/ + binaries into a versioned .tar.gz

make package-host   # host platform only — faster local iteration
make verify-package # build + validate the five-platform archive
```

Both stage `manifest.yaml` + the production `ui/bundle.js` alongside the freshly built
`server/plugin-<goos>-<goarch>[.exe]` binaries, then pack the tree with
kandev's `cmd/plugin-pack`, which computes `checksums.txt` and writes the
tarball. UI tests and Playwright sources are deliberately excluded from the
runtime archive.

Note the Makefile runs `plugin-pack` with `cd $(KANDEV_SDK) && go run
./cmd/plugin-pack`, from inside the sibling kandev checkout, rather than as
`go run github.com/kandev/kandev/cmd/plugin-pack` from here. The second
spelling resolves plugin-pack's dependencies against *this* module's `go.sum`,
and plugin-pack reaches much further into the kandev backend than `server/`
does — so those entries are missing and packaging fails with `missing go.sum
entry`. Pulling them in would force this repo's `go.sum` to track every
dependency the kandev backend grows. Building the tool where it lives keeps
`go.sum` scoped to what this plugin actually imports.

## Install it against a running kandev

Either through the UI (**Settings > Plugins > Install plugin**, URL or file
upload), or directly:

```sh
curl -F package=@kandev-plugin-redmine-0.7.0.tar.gz \
  http://localhost:<kandev-port>/api/plugins/install
```

kandev verifies `checksums.txt`, validates the manifest, extracts the package,
spawns the host-matching binary, and — once the go-plugin handshake completes —
marks the plugin active. Sideloaded plugins register **disabled/unverified**;
enable it in **Settings > Plugins** (the `plugins` feature flag must be on).
Reinstalling the same version returns 409 — bump `version` in `manifest.yaml`.

### Manual updates by URL

After the first release from this fork, paste this same URL into **Settings >
Plugins > Install plugin > URL** for each manual update:

```text
https://github.com/FilipK-CZ/kandev-plugin-redmine/releases/latest/download/kandev-plugin-redmine.tar.gz
```

GitHub resolves it to the latest published release. It does not point at CI
artifacts or unreleased changes on `main`. Each new release has a new manifest
version; installing the already installed version still returns 409.

The package keeps the `kandev-plugin-redmine` ID so it updates the existing
installation and retains its configuration, connections, and task links. No
GitHub credentials, marketplace source, or auto-update setting is needed.
Keep marketplace auto-update disabled for this fork and use this URL rather
than the marketplace's **Update** button, which resolves the original upstream
plugin under the same ID.

## Selective status write-back

In **Field mapping → Tracker → new task workflow**, enable **Write mapped status
changes to Redmine for this tracker** for each route that should report its
progress. With global **Automatic status write-back** off, only those
tracker/workflow routes write their mapped native task moves to Redmine.
Automatic intake remains a separate switch for starting the task and setting its
initial working status. Existing routes default to write-back off; customer
reply approval is independent. See [tracker workflows](docs/tracker-workflows.md).

## Publish a release

Pull requests run `.github/workflows/ci.yml` (tidy, format, vet, and test) and
`.github/workflows/build.yml` (host build plus a five-platform package). After
merging, run **Actions > release > Run workflow** on `main` with the desired
version bump. This commits the release version, tags it, then builds and
publishes it. Set `dry_run` to preview the next version without publishing.
Alternatively, push a tag that matches the manifest version. The release
workflow repeats verification, cross-compiles all platforms, packs the tarball,
and creates a GitHub Release with these assets for direct installation and the
[marketplace](https://github.com/kdlbs/kandev/blob/main/docs/public/plugins-marketplace.md)
install pipeline:

- `<id>-<version>.tar.gz` — the plugin package (with its own internal
  `checksums.txt` verified on install).
- `<id>.tar.gz` — a byte-identical copy with a stable asset name for the manual
  update URL above.
- `checksums.txt` — SHA-256 checksums of both downloadable archives. These are
  separate from the file checksums inside each archive.

```sh
# bump VERSION in Makefile + version in manifest.yaml first, then:
git tag v0.2.0
git push origin v0.2.0
```

The workflows check out the kandev monorepo as a sibling so the local Go SDK
path resolves (see "Developing against the SDK"). They pin one source revision
for a reproducible contract; advance that pin deliberately and rerun tests
when adopting a newer SDK.

## License

MIT — see [LICENSE](LICENSE).
