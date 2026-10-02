# Troubleshooting

> **Retired.** This page documented the original supervisor/tmux/systemd
> deployment (`bin/supervisor.sh`, `systemctl`, `/etc/hive/agent.env`), which is
> no longer how Hive runs. None of it applies to the current containerized Go
> deployment.
>
> **The current guide lives at
> [`src/docs/troubleshooting.md`](../src/docs/troubleshooting.md)** — container
> logs, config validation, agent sessions, dashboard auth, and GitHub
> credential checks.
>
> See the [`src/docs/README.md`](../src/docs/README.md) index for the full
> documentation set.

We removed the v1 content. It was searchable and sent operators to obsolete
`systemctl` steps. Recover it from git history if you maintain a v1 deployment:

```sh
git log --all --oneline -- docs/troubleshooting.md
git show <commit>:docs/troubleshooting.md
```
