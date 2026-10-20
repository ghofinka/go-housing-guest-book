# CLAUDE.md

@AGENTS.md

## Claude Code notes

- [AGENTS.md](AGENTS.md) is the single source of truth for stack, layout, domain rules and conventions. Edit it there, not here, so the two files do not drift.
- [PRD.md](PRD.md) is written in Indonesian. Treat it as the product spec; cite requirement IDs (M1, S3, ...) in commit messages and PR descriptions when implementing them.
- The repo is greenfield. When scaffolding, create the Makefile targets, `sqlc.yaml`, first migration and `openapi.yaml` described in AGENTS.md rather than inventing different ones.
- Shell on this machine is Windows (PowerShell, plus Git Bash). Keep Makefile and scripts portable, or provide a PowerShell alternative, and use forward slashes in Go paths.
- Docker is needed for the integration tests; if it is unavailable, say so rather than skipping or mocking the database.
- Before implementing a feature that touches visit or photo data, state how it enforces per-house isolation and how it is audited. These are the PRD's highest-risk requirements.
- Plan first (plan mode) for anything that changes the schema, auth model or retention behavior.
- Create log for every changes made as a markdown file with format ddmmyyyyhhmm