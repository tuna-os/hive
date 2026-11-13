# Tuna OS Fleet Deployment & Custom Agent Personas

This directory contains the production-ready configuration and custom agent persona definitions for the [Tuna OS](https://tunaos.org) organization (`hive.tunaos.org`).

For the full architectural investigation and design rationale, see [`docs/tuna-os-agent-personas.md`](../../docs/tuna-os-agent-personas.md).

---

## Architecture Overview

The Tuna OS Hive fleet manages 62 repositories covering immutable container OS image generation (`bootc`), cross-distro package factory maintenance, multi-frontend installers, native desktop applications (GTK4/libadwaita, Qt6, COSMIC/Iced, Rust), and upstream fork synchronization.

```
                    ┌────────────────────────┐
                    │      hive.tunaos.org   │
                    │   (Supervisor + Gov)   │
                    └───────────┬────────────┘
                                │
        ┌───────────────────────┼───────────────────────┐
        │                       │                       │
 ┌──────┴───────┐        ┌──────┴───────┐        ┌──────┴───────┐
 │ bootc-curator│        │   packager   │        │ desktop-     │
 │      🐟      │        │      📦      │        │ integrator 🎨│
 └──────┬───────┘        └──────┬───────┘        └──────┬───────┘
        │                       │                       │
        ▼                       ▼                       ▼
  tunaOS, tromso,         tunaos-packages,        blueshell,
  xfce-linux, remora      debian-copr, flatpak    gtk-office, mandelbrot
        │                       │                       │
 ┌──────┴───────┐        ┌──────┴───────┐        ┌──────┴───────┐
 │ installer-qa │        │ upstream-    │        │ desktop-     │
 │      💿      │        │ shepherd  🐑 │        │ advocate  📣 │
 └──────┬───────┘        └──────┬───────┘        └──────┬───────┘
        ▼                       ▼                       ▼
  tuna-installer-*,       hive, blueshell,        docs, branding,
  wootc, asahi, fisherman mandelbrot, mariner     flatpak-index
```

---

## Directory Structure

| Path | Description |
|---|---|
| [`hive-project.yaml`](hive-project.yaml) | Fleet configuration: repos, deterministic pipelines, classification rules, governor cadences |
| [`agents/bootc-curator.md`](agents/bootc-curator.md) | Policy prompt for OCI bootc image and layer maintenance |
| [`agents/bootc-curator.yaml`](agents/bootc-curator.yaml) | Portable `AgentDefinition` for `bootc-curator` |
| [`agents/packager.md`](agents/packager.md) | Policy prompt for RPM/DEB specfiles, BuildStream `.bst`, and Flatpak indexing |
| [`agents/packager.yaml`](agents/packager.yaml) | Portable `AgentDefinition` for `packager` |
| [`agents/desktop-integrator.md`](agents/desktop-integrator.md) | Policy prompt for GTK4/libadwaita/Qt6/COSMIC apps and GNOME HIG adherence |
| [`agents/desktop-integrator.yaml`](agents/desktop-integrator.yaml) | Portable `AgentDefinition` for `desktop-integrator` |
| [`agents/installer-qa.md`](agents/installer-qa.md) | Policy prompt for multi-frontend installers, EFI/systemd-boot, and Windows/Asahi |
| [`agents/installer-qa.yaml`](agents/installer-qa.yaml) | Portable `AgentDefinition` for `installer-qa` |
| [`agents/upstream-shepherd.md`](agents/upstream-shepherd.md) | Policy prompt for upstream fork synchronization and rebase maintenance |
| [`agents/upstream-shepherd.yaml`](agents/upstream-shepherd.yaml) | Portable `AgentDefinition` for `upstream-shepherd` |
| [`agents/desktop-advocate.md`](agents/desktop-advocate.md) | Policy prompt for desktop Linux community outreach and Flathub reach |
| [`agents/desktop-advocate.yaml`](agents/desktop-advocate.yaml) | Portable `AgentDefinition` for `desktop-advocate` |

---

## Deployment & Setup

### 1. Configure the Hive Server

Copy `hive-project.yaml` to `/etc/hive/hive-project.yaml` or mount it into your Hive container:

```bash
sudo cp hive-project.yaml /etc/hive/hive-project.yaml
```

### 2. Import Agent Definitions

Import individual agents into the dashboard (⚙️ → Import tab) or via the API:

```bash
for agent in bootc-curator packager desktop-integrator installer-qa upstream-shepherd desktop-advocate; do
  curl -X POST -H "Content-Type: application/yaml" \
    --data-binary @agents/${agent}.yaml \
    http://localhost:3001/api/agents/import
done
```

### 3. Progressive ACMM Rollout

1. **Advisory (ACMM L2):** Run all agents in `mode: ADVISORY` to evaluate work beads and findings in the dashboard.
2. **Issue Screening (ACMM L4):** Enable `ISSUES_ONLY` on `bootc-curator`, `packager`, and `upstream-shepherd`.
3. **Semi-Autonomous PRs (ACMM L5):** Enable `ISSUES_AND_PRS` with automatic `hold` labels. Maintainers batch-review and merge PRs carrying `git commit -s` signoffs.
