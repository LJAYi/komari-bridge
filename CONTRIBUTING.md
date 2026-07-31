# Contributing

Thank you for contributing to `komari-bridge`.

## Before opening a change

1. Keep collectors read-only and use fixed embedded commands.
2. Never commit real credentials, hostnames, IP addresses, SSH fingerprints,
   infrastructure labels, database files, or production configuration.
3. Add tests for provider parsing, identity stability, and fallback behavior.
4. Document any new privileges required on monitored systems.

## Local checks

```bash
gofmt -w .
go vet ./...
go test ./...
```

Integration tests must remain opt-in and must not contain real connection
details. Pull requests should explain security tradeoffs and how failure falls
back without taking unrelated resources offline.
