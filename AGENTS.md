# Agent Instructions for act

This repository is a Go-based CLI tool for running GitHub Actions locally. The codebase is organized around CLI parsing in `cmd/`, workflow planning in `pkg/model/`, action execution in `pkg/runner/`, Docker/container integration in `pkg/container/`, and shared utilities in `pkg/common/`.

## What to know first

- `act` is a developer tool, not a web app. Most changes will be in Go code and tests.
- The repository uses standard Go tooling and GitHub Actions workflow semantics.
- Avoid guessing the runtime environment: rely on the existing `Makefile`, `README.md`, and `CONTRIBUTING.md` for commands and conventions.

## Recommended commands

Use these commands to validate changes locally before suggesting edits:

- `make test` — runs `go test ./...` and CLI tests
- `make build` — builds the binary to `dist/local/act`
- `make lint-go` — runs `golangci-lint run`
- `make format` — runs `go fmt ./...`
- `make tidy` — runs `go mod tidy`
- `make pr` — cleanup, format, lint, and test for PR readiness

## Project conventions

- Keep Go code idiomatic and formatted with `go fmt ./...`.
- Do not add dependencies outside `go.mod`.
- Prefer small, focused changes in the relevant package instead of broad repository-wide rewrites.
- When modifying behavior, add or update tests under the same package and relevant `testdata/` fixture directories.

## Key areas

- `cmd/` — command-line interface definitions, flags, and CLI glue
- `pkg/model/` — workflow/job planning, GitHub Actions context, and model validation
- `pkg/runner/` — step execution, action handling, composite/reusable workflows, and test coverage for runner behavior
- `pkg/container/` — Docker operation wrappers, container/network/volume management, and platform-specific support
- `pkg/common/` — executor patterns, context management, logging, and helper utilities

## Useful docs

- `README.md` — repository overview, install/build instructions, and usage examples
- `CONTRIBUTING.md` — contribution workflow, style guide, and PR expectations
- `CLAUDE.md` — existing local guidance and common commands for this repository

## When in doubt

- Prefer preserving existing behavior unless a change is clearly warranted.
- Ask for clarification on broad design or behavior changes.
- If a suggested change affects test execution or Docker integration, verify with `make test` and the relevant package tests.
