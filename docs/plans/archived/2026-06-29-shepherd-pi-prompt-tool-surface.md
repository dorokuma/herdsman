# Herdsman Pi Prompt and Tool Surface Implementation Plan

> **For implementers:** Execute this plan task-by-task. Complete each checkbox step, run the listed validation, and commit after each task.

**Goal:** Make `herdsman-pi` teach Pi that Herdsman is a Herdr orchestration control-plane by moving always-on guidance into the extension, keeping the Herdsman skill as explicit optional documentation, and simplifying the Pi-visible logical tool surface.

**Architecture:** `herdsman-pi` owns Pi runtime behavior: attached-session hidden context, prompt cleanup, and dynamic tool registration. Herdsman Gateway owns logical tool metadata because the tool meaning lives with the logical tool definitions and can be reused by Pi and future adapters. The optional Herdsman skill remains package documentation for `/skill:herdsman`, not the primary prompt path.

**Tech Stack:** TypeScript ESM with NodeNext, TypeBox logical tool schemas, Vitest, Biome, Pi extension JavaScript package, Pi Agent Skills frontmatter.

**Status:** Done

**Progress:**
- Done — Herdsman skill frontmatter, optional skill guidance, Gateway logical tool prompt metadata, canonical Herdr tool names, `herdsman-pi` hidden context cleanup, and dynamic Pi tool metadata registration are implemented.
- Verified — `pnpm check`, `pnpm build`, `pi install ./packages/herdsman-pi`, Gateway restart/status, `herdsman-tools` `tool.list` metadata inspection, and CLI send/audit/rename/watch dogfooding passed.

**Next steps:**
- Archived for historical reference.

## Global Constraints

- Plan-only session constraint: this plan is the only file changed during plan writing. Do not edit implementation files until the user explicitly asks to execute the plan.
- Herdsman is the Herdr orchestration control-plane. Pi owns model/provider/session conversation behavior. Herdr is the execution surface for workspaces, tabs, panes, agents, logs, tests, and shells.
- The Herdsman skill remains in `packages/herdsman-pi`, but it is hidden from normal model invocation with `disable-model-invocation: true`.
- Runtime behavior must not depend on the skill body being read. The extension and tool metadata must be sufficient for normal attached sessions.
- Intentional overlap is limited to these core principles:
  - Herdsman is a Herdr orchestration control-plane.
  - Prefer `herdsman_*` tools when attached to a Herdsman session.
  - Do not expose Herdsman session ids, Gateway run ids, or other internal metadata unless the user asks.
- `promptSnippet` is added for every Pi-visible logical tool.
- `promptGuidelines` is added only for important behavioral rules and safety boundaries. Pi flattens these guidelines into the system prompt, so each guideline must name the affected `herdsman_*` tool or tool family.
- Compatibility with old alias tool names is not required. Remove alias registrations instead of hiding them.
- Canonical agent-related Herdr tool names use the `herdr_*` form:
  - keep `herdr_start_agent`, remove `start_agent`
  - keep `herdr_send_agent_message`, remove `send_agent_message`
  - keep `herdr_read_agent`, remove `read_agent_output`
  - keep `ensure_herdr_workspace`, remove `ensure_agent_pane`
- Do not update archived plan files to rewrite history. Update active docs only if they currently advertise the removed aliases.
- After implementation changes, run `pnpm check`. Because `packages/herdsman-pi` is package-validated and the change touches extension/package behavior, also run `pnpm pi-package:check`. Run `pnpm build` if TypeScript public types or compiled entrypoints change.

## Current Context

- `packages/herdsman-pi/package.json` declares both `pi.extensions` and `pi.skills`.
- `packages/herdsman-pi/skills/herdsman/SKILL.md` currently has no frontmatter, causing Pi to warn that `description is required`.
- Pi skill discovery requires `description`; skills with `disable-model-invocation: true` are loaded as commands but excluded from `<available_skills>` in the system prompt.
- `packages/herdsman-pi/extensions/index.js` currently injects a `herdsman.context` hidden message in `before_agent_start` and appends a short system prompt sentence, but it does not remove older `herdsman.context` messages from accumulated context.
- `registerHerdsmanTools` currently maps every Gateway tool to Pi with generic metadata:
  - `label: Herdsman ${tool.name}`
  - `promptSnippet: Delegate ${tool.name} to the attached Herdsman Gateway`
  - `promptGuidelines: Use herdsman_${tool.name} when the task needs Herdsman or Herdr orchestration.`
- `src/gateway/tools.ts` defines `LogicalToolDefinition` with `name`, `description`, `inputSchema`, and `execute` only.
- `src/gateway/server.ts#tool.list` currently returns `{ name, description, inputSchema }`.
- `src/tui/client.ts` exposes `ToolDefinitionWireRecord` with `{ name, description, inputSchema }`.
- `src/gateway/builtin-tools.ts` currently registers alias tools: `ensure_agent_pane`, `start_agent`, `send_agent_message`, and `read_agent_output`.
- `test/integration/builtin-tools.test.ts` contains a test named `plan-name aliases and pane tools delegate to Herdr` that must be rewritten because alias compatibility is intentionally removed.
- Herdr official skill uses frontmatter and practical instructions. Herdsman should follow the frontmatter convention but keep runtime guidance in the extension/tool metadata.

## File Structure

- Modify: `packages/herdsman-pi/skills/herdsman/SKILL.md` — add valid skill frontmatter and concise optional guidance.
- Modify: `packages/herdsman-pi/extensions/index.js` — update hidden context, remove stale Herdsman hidden messages via `context` hook, consume Gateway tool metadata.
- Modify: `src/gateway/tools.ts` — extend `LogicalToolDefinition` with optional Pi prompt metadata.
- Modify: `src/gateway/server.ts` — include optional metadata in `tool.list` results.
- Modify: `src/tui/client.ts` — extend `ToolDefinitionWireRecord` with optional metadata fields.
- Modify: `src/gateway/builtin-tools.ts` — add per-tool `promptSnippet`, selected `promptGuidelines`, and remove alias registrations.
- Modify: `src/cli/herdsman-tools.ts` — no behavior change expected, but keep wire passthrough compatible with added metadata.
- Test: `test/integration/gateway-rpc.test.ts` — assert `tool.list` exposes optional metadata.
- Test: `test/integration/builtin-tools.test.ts` — assert canonical Herdr tools remain and alias names are gone.
- Test: `test/unit/herdsman-tools.test.ts` — update fake tool metadata passthrough expectations.
- Optional test if existing harness makes it practical: `test/unit/herdsman-pi-extension.test.ts` or a new small extension test — assert `registerTool` receives Gateway metadata and fallback metadata remains for older Gateway responses. If no extension harness exists, cover this with `pnpm pi-package:check` and manual dogfooding.

## Tasks

### Task 1: Make the Herdsman skill valid optional documentation

**Objective:** Remove the Pi skill warning while keeping the skill as explicit `/skill:herdsman` documentation rather than normal prompt content.

**Files:**
- Modify: `packages/herdsman-pi/skills/herdsman/SKILL.md`

**Interfaces:**
- Produces: a Pi skill named `herdsman` with `disable-model-invocation: true`.
- Consumes: Pi Agent Skills frontmatter rules.

- [ ] **Step 1: Write the failing package check expectation**

Run the current package check before editing:

```bash
pnpm pi-package:check
```

Expected before implementation: the command may pass because `npm pack` does not validate Pi skill frontmatter, but interactive Pi currently emits `[Skill conflicts] ... description is required`. Record this as the user-visible failure this task fixes.

- [ ] **Step 2: Add skill frontmatter**

Change `packages/herdsman-pi/skills/herdsman/SKILL.md` to start with this frontmatter:

```md
---
name: herdsman
description: Guidance for using Herdsman as a Herdr orchestration control-plane from an attached Pi session. Use explicitly when you need Herdsman/Herdr role boundaries, orchestration principles, or package-level bridge behavior.
disable-model-invocation: true
---
```

Keep the `description` under 1024 characters. Do not add `allowed-tools` because this skill is documentation guidance, not a workflow that needs pre-approved tools.

- [ ] **Step 3: Rewrite the skill body as concise optional guidance**

Replace the current body with sections that cover:

```md
# Herdsman Bridge

Herdsman is a Herdr orchestration control-plane. Pi owns the model conversation and provider runtime; Herdr owns terminal execution surfaces; Herdsman binds platform messages, sessions, and Herdr resources together.

When attached to Herdsman:

- Prefer `herdsman_*` tools for Herdsman session inspection and Herdr orchestration.
- Use Herdsman logical tools instead of raw Herdr control unless the user explicitly asks for direct Herdr work.
- Treat Herdsman session ids, Gateway run ids, socket paths, and owner ids as internal metadata. Do not show them unless the user asks.
- Inspect current Herdsman/Herdr state before creating new workspaces, panes, or agents when the user asks for coordination.
- Non-Herdsman Herdr resources are user-owned. Attach to them only when the user explicitly asks.

The `herdsman-pi` extension injects current attached-session context and registers dynamic `herdsman_*` tools. This skill is a reference for role boundaries; normal attached sessions should rely on the extension and tool descriptions.
```

- [ ] **Step 4: Verify package syntax**

Run:

```bash
pnpm pi-package:check
```

Expected: command exits 0.

- [ ] **Step 5: Commit**

```bash
git add packages/herdsman-pi/skills/herdsman/SKILL.md
git commit -m "fix(pi): make herdsman skill optional documentation"
```

### Task 2: Extend logical tool metadata and Gateway wire output

**Objective:** Let Gateway logical tools define Pi-facing prompt metadata at the source of truth and expose it through `tool.list`.

**Files:**
- Modify: `src/gateway/tools.ts`
- Modify: `src/gateway/server.ts`
- Modify: `src/tui/client.ts`
- Modify: `test/integration/gateway-rpc.test.ts`
- Modify: `test/unit/herdsman-tools.test.ts`

**Interfaces:**
- Extends `LogicalToolDefinition<Input, Output>`:
  ```ts
  label?: string;
  promptGuidelines?: string[];
  promptSnippet?: string;
  ```
- Extends `tool.list` wire records:
  ```ts
  {
    description: string;
    inputSchema: unknown;
    label?: string;
    name: string;
    promptGuidelines?: string[];
    promptSnippet?: string;
  }
  ```

- [ ] **Step 1: Write failing Gateway RPC metadata test**

In `test/integration/gateway-rpc.test.ts`, update the `lists and runs logical tools through RPC` registry tool:

```ts
registry.register({
  description: "Echo a message",
  execute: (input: { text: string }) => ({ echoed: input.text }),
  inputSchema: Type.Object({ text: Type.String() }),
  label: "Echo message",
  name: "echo",
  promptGuidelines: ["Use herdsman_echo only when a test needs an echo response."],
  promptSnippet: "Echo a message through the Herdsman Gateway.",
});
```

Update the `tool.list` expectation to include:

```ts
{
  description: "Echo a message",
  inputSchema: expect.any(Object),
  label: "Echo message",
  name: "echo",
  promptGuidelines: ["Use herdsman_echo only when a test needs an echo response."],
  promptSnippet: "Echo a message through the Herdsman Gateway.",
}
```

Use `toMatchObject` if the existing exact equality makes the TypeBox schema shape noisy.

- [ ] **Step 2: Write failing Herdsman tools passthrough test**

In `test/unit/herdsman-tools.test.ts`, update `fakeClient().listTools()` and expected results so the fake `echo` tool includes `label`, `promptSnippet`, and `promptGuidelines`. This asserts the stdio helper does not strip metadata.

- [ ] **Step 3: Run targeted tests to verify failure**

Run:

```bash
pnpm test -- test/integration/gateway-rpc.test.ts test/unit/herdsman-tools.test.ts
```

Expected before implementation: TypeScript or assertions fail because `LogicalToolDefinition` and `ToolDefinitionWireRecord` do not include the new fields and `tool.list` does not emit them.

- [ ] **Step 4: Extend TypeScript types**

In `src/gateway/tools.ts`, update `LogicalToolDefinition`:

```ts
export type LogicalToolDefinition<Input, Output> = {
  description: string;
  execute: (input: Input, context: LogicalToolContext) => Promise<Output> | Output;
  inputSchema: TSchema;
  label?: string;
  name: string;
  promptGuidelines?: string[];
  promptSnippet?: string;
};
```

In `src/tui/client.ts`, update `ToolDefinitionWireRecord`:

```ts
export type ToolDefinitionWireRecord = {
  description: string;
  inputSchema: unknown;
  label?: string;
  name: string;
  promptGuidelines?: string[];
  promptSnippet?: string;
};
```

Do not require these fields because older or external logical tools may omit them.

- [ ] **Step 5: Emit metadata from `tool.list`**

In `src/gateway/server.ts#listTools`, include optional fields without changing `tool.run`:

```ts
tools: this.#logicalTools.list().map((tool) => ({
  description: tool.description,
  inputSchema: tool.inputSchema,
  ...(tool.label !== undefined ? { label: tool.label } : {}),
  name: tool.name,
  ...(tool.promptGuidelines !== undefined ? { promptGuidelines: tool.promptGuidelines } : {}),
  ...(tool.promptSnippet !== undefined ? { promptSnippet: tool.promptSnippet } : {}),
})),
```

- [ ] **Step 6: Run targeted tests**

Run:

```bash
pnpm test -- test/integration/gateway-rpc.test.ts test/unit/herdsman-tools.test.ts
```

Expected: both test files pass.

- [ ] **Step 7: Commit**

```bash
git add src/gateway/tools.ts src/gateway/server.ts src/tui/client.ts test/integration/gateway-rpc.test.ts test/unit/herdsman-tools.test.ts
git commit -m "feat(gateway): expose logical tool prompt metadata"
```

### Task 3: Add canonical builtin tool metadata and remove aliases

**Objective:** Make Pi-visible Herdsman tools clear and compact by removing compatibility aliases and adding source-of-truth prompt metadata to builtin tools.

**Files:**
- Modify: `src/gateway/builtin-tools.ts`
- Modify: `test/integration/builtin-tools.test.ts`
- Optional search-driven docs updates: `README.md`, `README.ja.md` only if active docs mention removed aliases.

**Interfaces:**
- Removes tool names from builtin registry:
  - `ensure_agent_pane`
  - `start_agent`
  - `send_agent_message`
  - `read_agent_output`
- Keeps canonical names:
  - `session_read`
  - `workspace_discovery`
  - `resolve_working_context`
  - `herdr_read`
  - `ensure_herdr_workspace`
  - `attach_herdr_workspace`
  - `herdr_start_agent`
  - `open_pane`
  - `run_pane_command`
  - `read_pane`
  - `send_pane_text`
  - `herdr_send_agent_message`
  - `herdr_read_agent`
  - `wait_for_agent`
  - `wait_for_herdr_event`

- [ ] **Step 1: Write failing alias removal tests**

In `test/integration/builtin-tools.test.ts`, replace the `plan-name aliases and pane tools delegate to Herdr` test with two tests.

First, assert the canonical names are registered:

```ts
test("builtin registry exposes canonical Herdsman/Herdr tools only", () => {
  const { runner } = openRunner();
  expect(runner.list().map((tool) => tool.name).sort()).toEqual([
    "attach_herdr_workspace",
    "ensure_herdr_workspace",
    "herdr_read",
    "herdr_read_agent",
    "herdr_send_agent_message",
    "herdr_start_agent",
    "open_pane",
    "read_pane",
    "resolve_working_context",
    "run_pane_command",
    "send_pane_text",
    "session_read",
    "wait_for_agent",
    "wait_for_herdr_event",
    "workspace_discovery",
  ].sort());
});
```

Second, keep the existing pane operation assertions using only canonical names. Do not call `start_agent`, `send_agent_message`, or `read_agent_output`.

- [ ] **Step 2: Write failing metadata assertions**

Add assertions in the canonical registry test that every tool has `promptSnippet`:

```ts
expect(runner.list().filter((tool) => !tool.promptSnippet)).toEqual([]);
```

Add targeted assertions for important guidelines:

```ts
const byName = new Map(runner.list().map((tool) => [tool.name, tool]));
expect(byName.get("resolve_working_context")?.promptGuidelines).toContain(
  "Use herdsman_resolve_working_context before creating Herdr resources when the working context is ambiguous.",
);
expect(byName.get("attach_herdr_workspace")?.promptGuidelines).toContain(
  "Use herdsman_attach_herdr_workspace only when the user explicitly asks to attach an existing non-Herdsman Herdr workspace.",
);
expect(byName.get("run_pane_command")?.promptGuidelines).toContain(
  "Use herdsman_run_pane_command only inside Herdsman-managed Herdr panes for tests, servers, logs, and controlled terminal workflows.",
);
```

- [ ] **Step 3: Run targeted test to verify failure**

Run:

```bash
pnpm test -- test/integration/builtin-tools.test.ts
```

Expected before implementation: fails because alias names still exist and builtin tools do not have prompt metadata.

- [ ] **Step 4: Remove alias registrations**

In `src/gateway/builtin-tools.ts`, delete the full `registry.register` blocks for:

- `ensure_agent_pane`
- `start_agent`
- `send_agent_message`
- `read_agent_output`

Do not leave dummy aliases or comments preserving old names.

- [ ] **Step 5: Add `promptSnippet` to every remaining builtin tool**

Use short one-line snippets that mention the `herdsman_` tool name after Pi registration. Suggested snippets:

- `session_read`: `Use herdsman_session_read to inspect recent Herdsman session events and orchestration history.`
- `workspace_discovery`: `Use herdsman_workspace_discovery to find known or allowed working contexts before binding Herdr resources.`
- `resolve_working_context`: `Use herdsman_resolve_working_context to resolve a project path, label, or slug into a Herdsman working context.`
- `herdr_read`: `Use herdsman_herdr_read to inspect Herdsman-bound Herdr workspaces, tabs, panes, and agents.`
- `ensure_herdr_workspace`: `Use herdsman_ensure_herdr_workspace to create or reuse the Herdsman-managed Herdr workspace for a task.`
- `attach_herdr_workspace`: `Use herdsman_attach_herdr_workspace to bind an explicitly requested existing Herdr workspace to the current Herdsman session.`
- `herdr_start_agent`: `Use herdsman_herdr_start_agent to start a configured worker agent inside the Herdsman-managed Herdr workspace.`
- `open_pane`: `Use herdsman_open_pane to open a Herdsman-managed Herdr pane for shells, logs, servers, or tests.`
- `run_pane_command`: `Use herdsman_run_pane_command to run a command in a Herdsman-managed Herdr pane.`
- `read_pane`: `Use herdsman_read_pane to read recent output from a Herdsman-managed Herdr pane.`
- `send_pane_text`: `Use herdsman_send_pane_text to send literal text to a Herdsman-managed Herdr pane.`
- `herdr_send_agent_message`: `Use herdsman_herdr_send_agent_message to send the user's task or follow-up to a Herdr-managed agent.`
- `herdr_read_agent`: `Use herdsman_herdr_read_agent to read recent output from a Herdr-managed agent.`
- `wait_for_agent`: `Use herdsman_wait_for_agent to wait for a Herdr-managed agent to become idle, done, blocked, working, or unknown.`
- `wait_for_herdr_event`: `Use herdsman_wait_for_herdr_event to wait for expected text in a Herdr pane before continuing.`

- [ ] **Step 6: Add selected `promptGuidelines`**

Add only the following high-signal guidelines:

- `resolve_working_context`:
  ```ts
  promptGuidelines: [
    "Use herdsman_resolve_working_context before creating Herdr resources when the working context is ambiguous.",
  ],
  ```
- `herdr_read`:
  ```ts
  promptGuidelines: [
    "Use herdsman_herdr_read to inspect current Herdr state before creating duplicate workspaces, panes, or agents.",
  ],
  ```
- `ensure_herdr_workspace`:
  ```ts
  promptGuidelines: [
    "Use herdsman_ensure_herdr_workspace for Herdsman-managed work; do not attach user-owned Herdr workspaces with it.",
  ],
  ```
- `attach_herdr_workspace`:
  ```ts
  promptGuidelines: [
    "Use herdsman_attach_herdr_workspace only when the user explicitly asks to attach an existing non-Herdsman Herdr workspace.",
  ],
  ```
- `herdr_start_agent`:
  ```ts
  promptGuidelines: [
    "Use herdsman_herdr_start_agent to delegate implementation or review work to configured Herdr worker agents instead of doing long-running work in Pi.",
  ],
  ```
- `run_pane_command`:
  ```ts
  promptGuidelines: [
    "Use herdsman_run_pane_command only inside Herdsman-managed Herdr panes for tests, servers, logs, and controlled terminal workflows.",
  ],
  ```
- `herdr_send_agent_message`:
  ```ts
  promptGuidelines: [
    "Use herdsman_herdr_send_agent_message for follow-up instructions to Herdr worker agents after reading their current state.",
  ],
  ```
- `wait_for_agent`:
  ```ts
  promptGuidelines: [
    "Use herdsman_wait_for_agent before summarizing delegated Herdr agent work when the agent may still be working.",
  ],
  ```

Do not add guidelines to every tool.

- [ ] **Step 7: Search active docs for removed aliases**

Run:

```bash
rg -n "ensure_agent_pane|start_agent|send_agent_message|read_agent_output" README.md README.ja.md docs/plans -g '!docs/plans/archived/**'
```

Expected: either no matches or only active docs that must be updated. If active docs mention aliases, replace them with canonical names. Do not edit archived plans.

- [ ] **Step 8: Run targeted tests**

Run:

```bash
pnpm test -- test/integration/builtin-tools.test.ts
```

Expected: test passes and the builtin registry exposes only canonical names.

- [ ] **Step 9: Commit**

```bash
git add src/gateway/builtin-tools.ts test/integration/builtin-tools.test.ts README.md README.ja.md
git commit -m "refactor(gateway): simplify Herdr logical tool names"
```

If README files were not changed, omit them from `git add`.

### Task 4: Update the Pi extension prompt injection and dynamic tool registration

**Objective:** Make `herdsman-pi` use Gateway tool metadata, inject concise attached-session behavior, and prevent stale hidden Herdsman context from accumulating.

**Files:**
- Modify: `packages/herdsman-pi/extensions/index.js`
- Optional test: add or modify extension tests if a local harness exists.

**Interfaces:**
- Consumes optional `tool.list` metadata:
  - `label?: string`
  - `promptSnippet?: string`
  - `promptGuidelines?: string[]`
- Produces Pi registered tools named `herdsman_${tool.name}` with Gateway-provided metadata and safe fallbacks.

- [ ] **Step 1: Inspect for an existing extension unit-test harness**

Run:

```bash
find test -type f | sort | rg "pi|extension|herdsman-pi"
```

Expected: if a harness exists, use it. If not, do not invent a large harness in this task; rely on `node --check`, package check, and dogfooding.

- [ ] **Step 2: Add stale context filtering**

In `packages/herdsman-pi/extensions/index.js`, add a `pi.on("context", ...)` handler near the existing lifecycle hooks. It should remove previous hidden Herdsman messages by `customType` and by a fallback marker in string content.

Implementation shape:

```js
pi.on("context", async (event) => ({
  messages: event.messages.filter((message) => {
    if (message?.customType === "herdsman.context") return false;
    if (message?.role !== "user") return true;
    const content = message.content;
    if (typeof content === "string") {
      return !content.includes("[SHEPHERD ATTACHED CONTEXT]");
    }
    if (Array.isArray(content)) {
      return !content.some(
        (part) => part?.type === "text" && part.text?.includes("[SHEPHERD ATTACHED CONTEXT]"),
      );
    }
    return true;
  }),
}));
```

Match the project’s plain JavaScript style. If Pi messages are not plain objects in this context, keep checks defensive.

- [ ] **Step 3: Update attached-session hidden context**

Change the `before_agent_start` hidden message content to start with a stable marker and include only concise B-scope guidance:

```text
[SHEPHERD ATTACHED CONTEXT]
Herdsman session id: <id>
Current Herdsman gateway run id: <id>    # only when present
Herdsman is a Herdr orchestration control-plane. Pi owns the model conversation; Herdr owns terminal execution surfaces.
Prefer herdsman_* tools for Herdsman session inspection and Herdr orchestration when attached.
Use Herdsman logical tools instead of raw Herdr control unless the user explicitly asks for direct Herdr work.
Do not expose Herdsman session ids, Gateway run ids, socket paths, or owner ids unless the user asks.
```

Keep `display: false`.

- [ ] **Step 4: Keep system prompt append short**

Replace the current appended system prompt sentence with a short non-duplicative boundary:

```text
When attached to Herdsman, hidden Herdsman context may include platform and orchestration metadata. Treat that metadata as internal unless the user asks for it.
```

Do not duplicate the full hidden context in both places.

- [ ] **Step 5: Use Gateway metadata in `registerHerdsmanTools`**

Update `pi.registerTool` mapping:

```js
const registeredName = `herdsman_${tool.name}`;
pi.registerTool({
  name: registeredName,
  label: tool.label ?? `Herdsman ${tool.name}`,
  description: tool.description,
  promptSnippet: tool.promptSnippet ?? `Use ${registeredName} through the attached Herdsman Gateway.`,
  promptGuidelines:
    Array.isArray(tool.promptGuidelines) && tool.promptGuidelines.length > 0
      ? tool.promptGuidelines
      : [`Use ${registeredName} when the task needs Herdsman session or Herdr orchestration.`],
  parameters: tool.inputSchema ?? { type: "object", additionalProperties: true },
  async execute(_toolCallId, params) {
    // existing implementation
  },
});
```

The fallback guideline must include the registered Pi tool name.

- [ ] **Step 6: Run syntax and package checks**

Run:

```bash
node --check packages/herdsman-pi/extensions/index.js
pnpm pi-package:check
```

Expected: both commands exit 0.

- [ ] **Step 7: Optional manual Pi verification**

After build/install in the final validation task, verify interactively:

```bash
pi install ./packages/herdsman-pi
```

Expected: no `[Skill conflicts] ... description is required` warning. Attached Herdsman sessions register `herdsman_*` tools with non-generic descriptions and guidelines.

- [ ] **Step 8: Commit**

```bash
git add packages/herdsman-pi/extensions/index.js
git commit -m "feat(pi): inject Herdsman orchestration guidance"
```

### Task 5: Full validation and dogfooding

**Objective:** Verify the prompt/tool surface change across typecheck, tests, package validation, build, and local Pi installation.

**Files:**
- No planned source edits. If validation exposes failures, fix the owning files from earlier tasks and rerun the relevant targeted tests.

**Interfaces:**
- Consumes all previous tasks.
- Produces a verified implementation ready for review.

- [ ] **Step 1: Run full check**

Run:

```bash
pnpm check
```

Expected: typecheck, Vitest, Biome, format check, Drizzle check, and `pi-package:check` pass.

- [ ] **Step 2: Run build**

Run:

```bash
pnpm build
```

Expected: TypeScript build and alias rewrite pass.

- [ ] **Step 3: Reinstall the Pi package locally**

Run:

```bash
pi install ./packages/herdsman-pi
```

Expected: install completes without the `description is required` warning for `packages/herdsman-pi/skills/herdsman/SKILL.md`.

- [ ] **Step 4: Restart Herdsman Gateway for dogfooding**

Run:

```bash
node dist/src/cli/herdsman.js gateway restart
node dist/src/cli/herdsman.js gateway status
```

Expected: status reports a running, reachable Gateway.

- [ ] **Step 5: Start a Herdsman-attached Pi session**

Run:

```bash
node dist/src/cli/herdsman.js
```

Expected: Pi starts, `herdsman-pi` attaches, and the TUI status shows `Herdsman <session-prefix>`.

- [ ] **Step 6: Smoke-test CLI delivery**

From another terminal, send a short message to the current session id:

```bash
node dist/src/cli/herdsman.js send <session-id> "Briefly confirm you are attached to Herdsman without exposing internal ids."
```

Expected: Pi responds naturally, does not echo Herdsman session id or Gateway run id, and can use `herdsman_*` tools if needed.

- [ ] **Step 7: Inspect tool list through Gateway RPC**

Run:

```bash
printf '{"id":"tools","method":"tool.list"}\n' | node dist/src/cli/herdsman-tools.js serve
```

Expected: the JSON Lines response has `id: "tools"`, no alias names, `promptSnippet` on every tool, and selected tools with `promptGuidelines`. The tool names are exactly:

```text
session_read
workspace_discovery
resolve_working_context
herdr_read
ensure_herdr_workspace
attach_herdr_workspace
herdr_start_agent
open_pane
run_pane_command
read_pane
send_pane_text
herdr_send_agent_message
herdr_read_agent
wait_for_agent
wait_for_herdr_event
```

- [ ] **Step 8: Commit validation fixes if any**

If validation required fixes, commit them with the relevant earlier task message style. If there are no fixes, do not create an empty commit.

## Validation

- `pnpm test -- test/integration/gateway-rpc.test.ts test/unit/herdsman-tools.test.ts` — verifies Gateway tool metadata wire output and stdio passthrough.
- `pnpm test -- test/integration/builtin-tools.test.ts` — verifies alias removal, canonical tool names, builtin metadata, and Herdr tool delegation.
- `node --check packages/herdsman-pi/extensions/index.js` — verifies extension syntax.
- `pnpm pi-package:check` — verifies extension syntax and package pack manifest.
- `pnpm check` — verifies typecheck, tests, lint, format, Drizzle check, and Pi package check.
- `pnpm build` — verifies compiled TypeScript and alias rewriting.
- `pi install ./packages/herdsman-pi` — verifies local Pi package installation and confirms the skill warning is gone.
- `node dist/src/cli/herdsman.js gateway status` — verifies the built CLI can see Gateway status during dogfooding.

## Risks, Tradeoffs, and Open Questions

- Removing alias tools is intentionally breaking. This is accepted because the current direction prioritizes a simple Pi-visible tool surface over backward compatibility.
- `promptGuidelines` are flat in Pi’s system prompt. Long or duplicate guidelines can pollute every turn, so this plan keeps guidelines limited and tool-name-specific.
- Skill text and extension hidden context intentionally overlap on three core principles. The overlap is limited so normal attached sessions are driven by extension/tool metadata while `/skill:herdsman` remains coherent when invoked manually.
- Stale hidden context filtering depends on Pi preserving `customType` in messages. The fallback marker `[SHEPHERD ATTACHED CONTEXT]` reduces risk if message shape changes.
- There is no confirmed unit-test harness for `packages/herdsman-pi/extensions/index.js`. If none exists, do not build a large test framework in this slice; verify with syntax/package checks and local Pi dogfooding.
- Archived plan files may still mention removed aliases. Leave archived history untouched unless the user asks for docs cleanup.
