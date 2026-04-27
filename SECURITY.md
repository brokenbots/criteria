# Security Policy

## Supported Versions

Overseer is currently pre-v1.0. Security fixes are applied to the latest
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

2. **Email:** Send details to the maintainers at the address listed in
   the GitHub org contact page. Encrypt with the maintainer's public PGP
   key if the details are sensitive.

Include as much detail as you can:

- A description of the vulnerability and its potential impact.
- Steps to reproduce or a minimal proof-of-concept.
- The version(s) affected (`overseer --version`).
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

In scope: the `overseer` CLI, the workflow execution engine, adapter plugin
protocol, SDK surface, and any bundled adapter plugins
(`overseer-adapter-noop`, `overseer-adapter-copilot`, `overseer-adapter-mcp`).

Out of scope: the Castle/Parapet orchestrator (report those to the
[overlord repo](https://github.com/brokenbots/overlord)), third-party
dependencies (report those upstream), and issues in example workflows that
do not affect the engine itself.
