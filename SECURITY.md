# Security Policy

## Supported Versions

Criteria is currently pre-v1.0. Security fixes are applied to the latest
minor release only. There is no long-term support promise before v1.0.

| Version | Supported |
|---------|-----------|
| latest  | ✅ Security fixes |
| older   | ❌ No backports |

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Please report security vulnerabilities through one of these channels:

1. **GitHub Security Advisory (preferred):** Use the
   [Security Advisories](../../security/advisories/new) page to file a
   private report. This is the fastest path to a coordinated fix.

2. **Email:** `security@brokenbots.net` — use this only if you cannot
   use GitHub Security Advisories. Encrypt with the maintainer's public
   PGP key if the details are sensitive.

Include as much detail as you can:

- A description of the vulnerability and its potential impact.
- Steps to reproduce or a minimal proof-of-concept.
- The version(s) affected (`criteria --version`).
- Any proposed remediation you have in mind.

## Disclosure Policy

- We follow a **90-day coordinated disclosure** window. We ask that you
  give us 90 days from the date of your report to release a fix before
  publishing details publicly.
- If coordinated disclosure is not possible (e.g., the issue is already
  public), please still notify us so we can expedite a fix.
- We will acknowledge receipt within 3 business days and aim to provide a
  status update within 14 days.
- We will credit reporters in the release notes unless you request
  anonymity.

## Scope

In scope: the `criteria` CLI, the workflow execution engine, adapter plugin
protocol, SDK surface, and any bundled adapter plugins
(`criteria-adapter-noop`, `criteria-adapter-copilot`, `criteria-adapter-mcp`).

Out of scope: the server/orchestrator (report those to the
[orchestrator repo](https://github.com/brokenbots/orchestrator)), third-party
dependencies (report those upstream), and issues in example workflows that
do not affect the engine itself.
