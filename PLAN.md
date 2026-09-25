# Goal

Let an agent discover and use tools omitted from the initial MCP catalog without editing the rules file during a session. Keep explicit `allow` tools visible and explicit `deny` tools inaccessible.

# Requirements

- Every configured MCP server uses `allow` / `deny` / automatic discovery, without a separate mode. An omitted `allow` means an empty baseline; absent entries retain pass-through behavior.
- `allow` tools are visible from startup, `deny` tools are never listed, described, activated, or called, and every other upstream tool is a discoverable candidate. Reject overlapping `allow` and `deny` names when loading rules.
- Always publish `search_tools`, `describe_tool`, and `call_tool` for a configured entry. Reserve these names and reject upstream collisions.
- `search_tools(query, limit)` returns at most ten deterministic matches with names, short summaries, and activation status. It does not activate tools. `describe_tool(name)` returns the selected tool's complete effective metadata and schema and activates it. `call_tool(name, arguments)` activates an auto candidate if necessary, then invokes it.
- Activation adds the original tool to the downstream `tools/list` for the lifetime of this MCP connection and emits `notifications/tools/list_changed`. Keep `call_tool` usable when a client does not refresh its model's tool set during a turn.
- A successful activation is in memory only. It does not modify `.mcp-filter.json` or `.mcp-filter.local.json`. A reconnect starts with the configured baseline.
- Log activation at `info` with entry, tool name, trigger (`describe_tool` or `call_tool`), and session/process identity. Never log arguments, results, schemas, or credentials.

# Non-goals

- Starting the upstream MCP server or fetching its initial tool catalog lazily. The first version only reduces the catalog sent to the agent.
- Semantic search or an external indexing service. Start with deterministic lexical matching over effective names, titles, and descriptions.
- Guaranteed immediate availability of newly advertised individual tools in every host. `call_tool` is the portable invocation path.

# Constraints

- Treat `call_tool` as a broad capability: host approval applies to the generic call, not to the hidden tool's individual name. Set explicit approval in the Codex example and document the Claude approval setting separately; auto-approving it grants access to every non-denied auto candidate. Recheck policy on every invocation.
- Validate `call_tool` arguments against the selected upstream tool schema before forwarding. Apply the existing timeout and preserve upstream results and errors.
- Keep one synchronized active-tool set. Config reload must revoke newly denied tools and re-evaluate previously activated names. Check the current policy immediately before forwarding; a call already dispatched upstream may complete after a reload.
- Apply existing metadata patches before indexing, describing, or publishing a tool. Keep helper tool names and descriptions stable for prompt caching.

# Acceptance criteria

1. With `allow: [a]` and `deny: [c]`, initial `tools/list` contains `a` plus the three helper tools, but neither `b` nor `c`.
2. Searching for `b` returns a short summary; searching for `c` does not reveal it. A search alone leaves `tools/list` unchanged.
3. Describing `b` returns its effective schema, adds `b` to `tools/list`, and emits one list-change notification. Repeated activation is idempotent.
4. Calling `b` through `call_tool` works before and after activation. Calling `c` or an unknown name never reaches upstream. Invalid arguments never reach upstream.
5. A second MCP connection starts from the baseline. No rules file changes after activation.
6. A hot reload that newly denies `b` removes it from the visible list and rejects calls whose authorization check occurs after reload. Document the already-dispatched call boundary.
7. Logs show which tool was activated and why, without arguments or results.
8. End-to-end checks in Codex and Claude verify `search_tools` → `describe_tool` → `call_tool` in one agent turn; direct tool refresh is recorded separately as host-dependent behavior.

# Implementation plan

1. Extend configuration types, merge behavior, schema, validation, examples, and migration documentation for `deny` and implicit discovery.
2. Move catalog selection and active-tool state into a synchronized per-connection component. Reconcile published tools on activation and rules reload; enforce policy again inside every handler.
3. Add the three helper tools, lexical search, schema description, argument validation, and generic forwarding using the existing timeout path.
4. Add structured activation and denial diagnostics without payload logging.
5. Cover policy, ranking, activation, notification, reload races, and reconnection with unit and MCP integration tests. Run live Claude and Codex CLI checks against an isolated fixture before enabling discovery in Postbox.

# Verification

- `go test ./...` and `git diff --check`.
- Isolated MCP client confirms the initial list, list-change notification, updated list, and direct/generic calls.
- Isolated Codex and Claude sessions confirm the portable generic path; Claude CLI needs an authenticated session for this check.

# Current result

- The MCP client and Codex CLI completed search → describe → call with the isolated fixture. Reconnecting reset the active set, and the log recorded activation.
- Claude CLI is not authenticated on this machine, so its live agent check remains pending. The proxy's MCP behavior is covered by the client integration test.
