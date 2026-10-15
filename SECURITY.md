# Security Policy

Opsward brokers access to infrastructure, so a flaw in it can be a flaw in everything
behind it. Please report vulnerabilities privately.

## Reporting

Use GitHub's **Report a vulnerability** button on the repository's Security tab. Do not open
a public issue, pull request or discussion for a suspected vulnerability.

Include the affected version or commit, a description of the impact, and steps to reproduce.

## Scope

Of particular interest:

- a principal invoking a tool, or reaching a resource, its policy does not grant
- secret values reaching tool output or audit records
- a way to make the engine run an arbitrary command or API call
- prompt-injected content causing a write without the configured approval

## Supported versions

Pre-1.0: only the latest release receives fixes.
