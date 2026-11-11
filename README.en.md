# Feishu Codex Bridge

[简体中文](README.md) | English

Built by AI. Iterated with AI. Maintained with AI. Feishu Codex Bridge turns Feishu into a workspace for Codex, carrying work from a request through implementation, testing, review, and maintenance preparation.

AI coding agents implement this project and continue to support feature iteration, debugging, documentation, and maintenance. People set direction, review outcomes, and authorize actions; agents work within explicit boundaries, with reviewable changes, recorded verification, and manual approvals.

Manage local Codex tasks from Feishu direct messages: switch projects and sessions, configure models, review operations, and retrieve files and complete results.

The bridge connects the official Feishu SDK over a persistent connection to a local Codex App Server. Session, task, approval, and file logic is separate from the frontend. Feishu is the currently implemented adapter.

## Features

- **Session topics:** keep each complete reply visible in the main chat, with a link to the session topic containing its conversation and results.
- **Multiple projects and sessions:** route messages through a default session, a quoted reply, or an explicit session reference.
- **Task controls:** inspect queued requests and submission times, cancel one waiting task, or request interruption of the active task.
- **Independent model settings:** configure bridge defaults or a complete session override; distinguish the current turn from the next.
- **Approvals and answers:** retain operation details, edit a draft, and submit it manually.
- **Attachments and results:** receive files and images, export project files, and retrieve complete long responses.
- **Handoff continuation:** carry the project, handoff document, and full model configuration into a new session.
- **Recovery and maintenance:** persist input and delivery receipts without automatically replaying work; update with idle checks, readiness verification, and rollback.

Only model inference requests run serially. Message transport, event handling, local controls, and tools do not require a single thread.

## Quick start

### 1. Prerequisites

- Go 1.24 or later, and Node.js for the project runner and maintenance scripts.
- An installed Codex CLI with the required authentication or provider configuration, supporting `codex app-server --listen stdio://`.
- An existing project directory and a Feishu application with bot capabilities.
- A Feishu persistent connection configured for `im.message.receive_v1` and `card.action.trigger`, with the necessary messaging and file resource permissions. The native command menu is optional.

The maintenance workflow currently targets Linux. No additional HTTP callback service is required. The host needs outbound access to Feishu and the configured model provider.

### 2. Build

Run from the repository root:

```sh
node scripts/project.mjs go build -buildvcs=false -trimpath -o .cache/artifacts/feishu-bridge ./cmd/feishu-bridge
```

The project runner keeps tool caches, temporary files, and build artifacts in this project's `.cache/` directory.

### 3. Create a private configuration

Copy the public example for a new deployment:

```sh
node scripts/project.mjs cp config.example.json .cache/config.json
node scripts/project.mjs chmod 600 .cache/config.json
```

Edit the new file yourself to provide application credentials and the project directory. Do not overwrite an existing deployment's configuration or commit credentials, tokens, session state, or caches.

| Field | Purpose |
|---|---|
| `app_id` / `app_secret` | Feishu credentials; `FEISHU_APP_ID` / `FEISHU_APP_SECRET` environment variables are also supported |
| `project_dir` | Existing default project directory |
| `state_dir` | Private state directory; relative paths resolve from the configuration file's directory and must not equal the project directory |
| `allowed_open_ids` | Authorized Feishu users; explicit pairing is also supported |
| `codex_binary` | Codex CLI executable, defaulting to `codex` |
| `codex_home` | Optional Codex home; leave empty to inherit the runtime environment |
| `catalog_homes` | Optional session catalogs from other clients; original records remain read-only |
| `model` | Optional startup model; an empty value resolves the default from the model catalog |
| `approval_policy` | `on-request` or `untrusted` |
| `sandbox` | `workspace-write` or `read-only` |
| `writable_roots` | Optional additional authorized directories, only with `workspace-write` |
| `turn_timeout_seconds` | Per-turn timeout, defaulting to 3600 seconds |

See [config.example.json](config.example.json) for the complete format and [directory authorization](docs/AUTHORIZATION.md) for details (Chinese).

### 4. Check and start

```sh
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json --check
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json
```

`--check` validates configuration and executable availability; it does not prove authentication, connectivity, or interaction readiness. To authorize through pairing instead of a preconfigured user list, use this startup command instead:

```sh
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json --pair
```

Follow the terminal prompt and send `/pair <code>` to the bot within ten minutes. These are alternative startup modes: do not start two instances against the same state directory. Once authorized, send `/start` to open the workspace menu.

## Usage

### Session routing

Ordinary messages use the default session. Quote a bot message to continue its session, or use an explicit reference such as `#3 Check the test results`. Precedence is **explicit reference → quoted message → default session**. An unknown quoted message never silently falls back to the default session.

Replies show the session reference, short project name, status snapshot, model, and reasoning effort. `/status #3` reads local state; `/show #3` includes the full directory and queries the model catalog to validate next-turn settings.

### Common commands

Replace example references, paths, and placeholders with actual values.

| Action | Command |
|---|---|
| Workspace menu / complete help | `/start` / `/help` |
| Choose a known project / browse sessions | `/projects` / `/sessions` |
| New session / another authorized directory | `/new Name` / `/new --cwd "/path/to/project" Name` |
| Change the default session | `/use #3` |
| Status / recent messages | `/status #3` / `/history #3` |
| Queue / cancel exactly one waiting task | `/queue` / `/cancelone <task-id>` |
| Cancel all queued work in a session / request interruption | `/cancel #3` / `/stop #3` |
| Available models / another settings page | `/models` / `/model #3 page 2` |
| Bridge-wide model / effort defaults | `/model global <model-id>` / `/effort global <effort>` |
| Session model / effort | `/model #3 <model-id>` / `/effort #3 <effort>` |
| Reset the complete session override | `/model #3 default` or `/effort #3 default` |
| Attachments and results / retrieve a complete result | `/files #3` / `/result <result-id>` |
| Export a project file | `/file #3 docs/report.md` |
| Generate a handoff / continue its project and settings | `/handoff #3` / `/continue #3` |
| Open session command drafts | `/commands #3` |

`/new` inherits the quoted or default session's project directory and uses bridge-wide model defaults. `/continue` checks that the source session is idle and its handoff document is readable, creates a new session with the source's full configuration pinned, and starts checking the handoff. Creating a session never expands directory permissions.

### Session topics and history

Each user, chat, and Codex session has a separate Feishu topic. Complete current replies remain visible in the main chat; text and cards link to the session topic. The topic collects user inputs, replies, progress, approval details, and results. Restarting or switching the default session preserves the binding. Reply within a topic to continue its session.

Inputs sent from the main chat are copied with clear attribution and never submitted to the model again. Inputs already inside the topic are not copied. Approvals still require the original user to submit them manually before expiry. Failed topic copies retain the outbox item and reuse the saved main-message receipt on retry.

Previously scattered messages are not automatically migrated into native replies; `/history #3` shows existing model history. Topic deep links and desktop/mobile presentation require real client acceptance. Other frontend adapters retain their existing delivery behavior.

### Models and reasoning effort

“Global” means defaults for this bridge, not machine-wide Codex configuration. The first customization of either session field pins **both** model and effort. Later global changes do not affect that session.

Setting either session field to `default` restores **both fields** to global inheritance. A `global` reset clears only its specified field. Settings persist and apply from the next turn; the current turn keeps its captured configuration, and queued work resolves settings when it starts. Available models and supported effort values come from `/models`.

### Approvals and files

Highlighted card actions open editable drafts. You can change the text or select another option; **manual submission is required**. The card editor is not Feishu's native chat composer. Sending a text command directly submits the operation. Approval and answer requests are bound to the original user, chat, session, and turn and expire after ten minutes; ordinary drafts generally expire after thirty minutes.

Send a file or image to save it under the target project's `.cache/feishu-files/` directory. Receiving it does not start inference. Subsequent tasks receive attachment metadata, not the full contents automatically. Files must be 1 byte to 20 MiB; up to four downloads can run concurrently. `/files` lists up to 20 recent attachments and registered results. Project files that have only been generated, not sent, can be registered and retrieved through `/file`.

Retrieve a complete long response with `/result`. Delivery uses snapshots, so retries do not reread a changed source file; result snapshots remain available for retrieval. File export excludes private configuration, credentials, session directories, and files outside authorized roots.

## Native commands and maintenance

Text commands and cards work independently of native command registration. Inspect or synchronize the native menu separately:

```sh
node scripts/project.mjs .cache/artifacts/feishu-bridge --slash-commands plan
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json --slash-commands check
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json --slash-commands sync
```

`plan` lists definitions offline; `check` performs a read-only inspection; `sync` creates missing definitions in Feishu and requires the corresponding permissions.

See [LAUNCH](LAUNCH.md) for installation, packaging, delayed updates, and rollback (Chinese). `scheduled` confirms scheduling only; `ready` with runtime hash verification confirms recovery. Card interactions, file delivery, and model switching require separate acceptance checks. Restart does not replay tasks or approvals, and old cards may become unavailable.

## Development and verification

```sh
node scripts/project.mjs go test -race -count=1 -timeout=60s ./...
node scripts/project.mjs go vet -tags integration ./...
node scripts/project.mjs node --test --test-isolation=none scripts/feishu-release.test.mjs scripts/feishu-update.test.mjs scripts/feishu-maintenance.test.mjs scripts/feishu-publication.test.mjs
node scripts/project.mjs node scripts/stage-feishu.mjs --binary .cache/artifacts/feishu-bridge
```

Build as described in Quick start before packaging. Default tests use synthetic data; the `integration` tag above is used only for static checks, without automatically running real inference or accessing an existing private configuration.

Additional documentation (Chinese):

- [Architecture and frontend integration](docs/ARCHITECTURE.md)
- [Verification and acceptance boundaries](VALIDATION.md)
- [Contributing and working with AI](CONTRIBUTING.md)

Only the Feishu adapter is currently wired. Local automated checks, deployment readiness, and real client acceptance are tracked separately.
