# Security policy

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting feature for this
repository. Do not open a public issue for a suspected credential leak,
authentication bypass, command-injection path, or privilege-escalation issue.

## Operational security

- Use dedicated, least-privilege PVE tokens and SSH keys.
- Pin SSH host keys and protect private keys with filesystem permissions.
- Store Komari automatic-discovery keys and provider secrets outside Git.
- Treat the SQLite database as sensitive because it stores Komari client
  tokens.
- Do not expose the extension API without authentication and transport
  security.
- Scope `VM.GuestAgent.Unrestricted` only to VMs configured for QGA guest
  memory collection.

No provider exposes arbitrary remote command execution through the bridge API.
Collectors use fixed commands embedded in the binary.
