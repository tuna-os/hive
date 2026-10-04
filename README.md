# Hive Infrastructure Tools

This repository contains core infrastructure scripts for the TunaOS Hive fleet — the automation system that manages rotation, healing, credential-sharing, and pacing across distributed hive instances.

## Overview

The hive infrastructure consists of:

- **`nous-hive-gate.py`** — Nous Gate implementation for experiment approval decisions
- **`nous-runner.sh`** — Agent runner script for executing hive workloads
- **`nous-sync.py`** — Synchronization and state management utility
- **`nous-install.sh`** — Installation and setup script

These tools bridge the hive's control plane (dashboard) with agent execution, implementing governance policies (observe/suggest/evolve modes) and credential management across the fleet.

## Nous Gate Protocol

The Nous Gate (`nous-hive-gate.py`) implements a three-mode approval decision system:

### Modes

- **`observe`**: Auto-approves all decisions (dry-run semantics handled upstream)
- **`suggest`**: Submits decision questions to the hive dashboard and long-polls for operator approval
- **`evolve`**: Auto-approves (autonomous loop for self-governing hives)

### Scopes

- **`governor`**: Uses the configured mode
- **`repo`**: Forces `suggest` mode regardless of settings (manual approval for repository-scoped work)

### Environment Variables

- `HIVE_DASHBOARD_URL` — URL to the hive dashboard (default: `http://localhost:3001`)
- `HIVE_DASHBOARD_TOKEN` — Authentication token for dashboard API
- `NOUS_HIVE_MODE` — Operating mode: `observe`, `suggest`, or `evolve` (default: `observe`)
- `NOUS_HIVE_SCOPE` — Scope: `governor` or `repo` (default: `governor`)
- `GATE_TIMEOUT_SEC` — Decision polling timeout in seconds (default: 1800)
- `GATE_POLL_INTERVAL_SEC` — Polling interval in seconds (default: 5)

## Usage

### Nous Gate

```bash
# Auto-approve mode
NOUS_HIVE_MODE=observe nous-hive-gate.py
  --question "Deploy new agent image?"
  --artifact-path /path/to/deployment.json

# Suggest mode (requires dashboard)
NOUS_HIVE_MODE=suggest \
HIVE_DASHBOARD_URL=https://hive.example.org \
HIVE_DASHBOARD_TOKEN=token_here \
nous-hive-gate.py
  --question "Promote to production?"
  --artifact-path /path/to/release.json

# Repo-scoped approval (always suggests)
NOUS_HIVE_SCOPE=repo \
HIVE_DASHBOARD_URL=https://hive.example.org \
HIVE_DASHBOARD_TOKEN=token_here \
nous-hive-gate.py \
  --question "Merge PR #123?"
  --artifact-path /path/to/pr-summary.json
```

### Runner

```bash
# Execute a hive task
./nous-runner.sh --task-id task_123 --config /etc/hive/config.yaml
```

### Sync

```bash
# Synchronize fleet state
./nous-sync.py --source production --target staging
```

## Installation

### Prerequisites

- Python 3.8+
- Bash 4.0+
- Network access to hive dashboard (for suggest mode)

### Setup

```bash
# Clone the repository
git clone https://github.com/tuna-os/hive.git
cd hive

# Make scripts executable
chmod +x bin/*.sh bin/*.py

# (Optional) Install to system path
sudo cp bin/* /usr/local/bin/
```

## Configuration

The hive infrastructure reads configuration from:

1. Environment variables (highest priority)
2. Configuration files (typically `/etc/hive/config.yaml` or similar)
3. Hardcoded defaults (lowest priority)

### Common Configuration

```bash
export HIVE_DASHBOARD_URL="https://hive.example.org"
export HIVE_DASHBOARD_TOKEN="secret_token_from_dashboard"
export NOUS_HIVE_MODE="suggest"  # or "observe" for testing
export NOUS_HIVE_SCOPE="governor"
```

## Monitoring and Logging

Each script logs to:

- `stdout`/`stderr` for immediate feedback
- System journal (via `journalctl`) when run as systemd service
- Log files as configured in hive dashboard settings

Enable verbose output:

```bash
# Python scripts
PYTHONUNBUFFERED=1 ./bin/nous-hive-gate.py ...

# Shell scripts
bash -x ./bin/nous-runner.sh ...
```

## Credentials and Security

### Dashboard Token

The `HIVE_DASHBOARD_TOKEN` grants read access to fleet status and is sent via the `Authorization: Bearer` header. Treat it as a secret:

- Do not commit to version control
- Do not include in logs or error messages
- Rotate periodically

### X-Hive-Internal Header

The hive uses `X-Hive-Internal` header for server-to-server reads (read-only). This credential is used when the dashboard blocks `Authorization: Bearer` tokens (on direct-route spokes with `dashboard.authorized_users` set).

## Development

### Testing

```bash
# Test the gate with observe mode (no dashboard needed)
NOUS_HIVE_MODE=observe python3 bin/nous-hive-gate.py \
  --question "Test?" \
  --artifact-path /tmp/test.json

# Test runner locally
./bin/nous-runner.sh --dry-run
```

### Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/name`)
3. Make your changes and test locally
4. Commit with DCO sign-off (`git commit -s`)
5. Push to your fork
6. Open a pull request

All commits must be signed off with the Developer Certificate of Origin (DCO).

## Troubleshooting

### Dashboard Connection Issues

```bash
# Test connectivity
curl -H "Authorization: Bearer $HIVE_DASHBOARD_TOKEN" \
  https://hive.example.org/api/health

# Check timeout settings
export GATE_TIMEOUT_SEC=3600  # Increase timeout for slow networks
```

### Token Errors

```bash
# Verify token is set and valid
echo $HIVE_DASHBOARD_TOKEN  # Should not be empty
curl -H "Authorization: Bearer $HIVE_DASHBOARD_TOKEN" \
  https://hive.example.org/api/widget
```

### Decision Polling Timeouts

If the gate times out waiting for decisions:

1. Check that the dashboard is running and accessible
2. Verify the hive is not in a paused state
3. Increase `GATE_TIMEOUT_SEC` if network latency is high
4. Check logs on the dashboard for operator activity

## Fleet Architecture

The scripts are designed to run on each hive spoke:

```
Hive Dashboard (control plane)
    ↓ (API)
HiveSpoke1 (hive instance)
    ├─ nous-hive-gate.py (decision gate)
    ├─ nous-runner.sh (agent executor)
    ├─ nous-sync.py (state sync)
    └─ agents (running workloads)
```

For fleet documentation, see the [hive-operator](https://github.com/tuna-os/hive-operator) repository, which manages these components on Kubernetes.

## License

These tools are part of the TunaOS project. See LICENSE for details.

## Support

- **Issues**: File bugs at https://github.com/tuna-os/hive/issues
- **Documentation**: See [hive-operator](https://github.com/tuna-os/hive-operator) for fleet-level documentation
- **Community**: Join [TunaOS discussions](https://github.com/tuna-os)
