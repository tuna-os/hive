# TunaOS Hive Hub Frontend Branding

This directory contains the customized frontend for the TunaOS Hive Hub (`https://hub.tunaos.org`).

- `index.html`: The complete custom TunaOS Hive Constellation single-page dashboard reskin.
- `activity.json`: Initial state snapshot for the live activity feed.
- `contributor-release.json`: Checksum metadata used by the compute contribution onboarding modal.

## Deployment Architecture

In production, the Hive Hub runs behind Caddy or an ingress proxy:
- `/api/*` and `/login/*` are reverse-proxied to the Hive Hub backend container on port 3001.
- All other routes (`/*`) are served from this static directory via nginx or Caddy's file server.
