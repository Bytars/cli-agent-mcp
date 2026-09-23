# Security policy

cli-agent-mcp starts coding agents on your machine, with your user's
permissions. A bug here can mean an agent doing something you did not ask
for, so we take reports seriously.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Report privately through GitHub:
[Security → Report a vulnerability](https://github.com/Bytars/cli-agent-mcp/security/advisories/new).

Include what you can of:

- the version (`cli-agent-mcp --version`) and OS;
- what an attacker needs (local user? a malicious repo? a web page?);
- steps to reproduce, and what happened versus what you expected.

We aim to acknowledge a report within 3 business days and to agree on a
disclosure date with you once the impact is understood. We will credit you in
the advisory unless you prefer otherwise.

## Supported versions

Only the latest release receives security fixes.

## Scope

In scope:

- the approval endpoint and the `ui` viewer (both bind to loopback and use
  per-task / per-session tokens) — anything that reaches them from another
  origin, another host, or without the token;
- escaping `CLI_AGENT_MCP_ALLOWED_CWDS`;
- the Claude deny list or plan-first mode not being applied;
- the release pipeline: a published binary or `.mcpb` that does not verify
  with `gh attestation verify`.

Out of scope, by design (see [Security model](README.md#security-model)):

- anything that requires already running code as the same OS user — that user
  can start the agents directly;
- what an agent does inside an allowed directory once you asked it to act.
  Kimi Code in headless mode runs tools without asking and has no deny list;
  restrict it with `CLI_AGENT_MCP_ALLOWED_CWDS`.

## Verifying a release

Every release binary and bundle is built by GitHub Actions and carries a
build-provenance attestation:

```sh
gh attestation verify cli-agent-mcp_windows_amd64.exe --repo Bytars/cli-agent-mcp
```

A file that does not verify was not built from this repository.
