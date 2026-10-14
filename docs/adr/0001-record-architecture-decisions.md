# 0001. Record architecture decisions

- Status: accepted
- Date: 2026-10-11

## Context

Decisions made in chat or in someone's head are lost. New contributors (and interviewers) ask *why* things are the
way they are.

## Decision

Record every significant decision as a short Architecture Decision Record in `docs/adr/`, numbered in order, using
[0000-template.md](0000-template.md). A decision is significant if it is hard to reverse or affects more than one
component.

## Consequences

- Reviews can point to the reasoning instead of re-arguing it.
- Superseded decisions stay in history with a link to the replacement.
