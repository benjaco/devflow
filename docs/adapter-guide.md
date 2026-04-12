# Adapter Guide

Projects integrate with Devflow by implementing `pkg/project.Project`.

An adapter defines:
- tasks
- targets
- instance configuration

Tasks should stay semantic. The adapter decides which files, directories, env vars, and custom probes contribute to each fingerprint.

The core engine does not know about Prisma, sqlc, Next.js, or any repo-specific conventions.
