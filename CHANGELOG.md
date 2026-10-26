# Changelog

## [0.4.0] - Unreleased

- Add configurable workflow-driven reply drafts with live public issue context.
- Add immutable operator previews and explicit public/private comment approval.
- Verify journal readback and retain uncertain attempts without repeating PUT.
- Hold the communication step against inbound status-sync moves until a human moves it.
- Keep reply generation disabled by default, credentials server-side, and human approval as the default publication mode.
- Add an explicit workspace switch for automatic publication of new verified drafts, with policy receipts, session/interaction checks and no backlog draining.
- Allow an explicit new draft after a completed refusal or malformed response, while active/uncertain work remains blocked.
- Load subsequent proposals without hiding them behind earlier publication receipts or resending the old text.
- Upload versioned installable CI packages for pull requests and main.

## [0.3.2] - 2026-09-14

### Changed

- fix: skip malformed possible values (2192918)
- Revert "release: 0.3.2" (079b7d3)
- fix: drop stdout Printf from possible_values decode (7231d93)
- release: 0.3.2 (00b26a5)
- fix: decode both Redmine possible_values wire formats (08182cf)


## [0.3.1] - 2026-09-14

### Changed

- fix: pin SDK contract to released kandev v0.94.0 (03b2754)
- fix: preflight released SDK contract (980dead)


## [0.3.0] - 2026-09-08

### Changed

- fix: repair Redmine mapping and sync behavior (92c54d3)
- test: align live Redmine watcher filters (e903d6f)
- fix: align Redmine mappings with Host SDK (0bdbd04)
- feat: complete Redmine integration candidate (#1) (9ab5677)

