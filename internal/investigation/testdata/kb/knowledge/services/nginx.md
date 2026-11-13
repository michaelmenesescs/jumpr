---
services: [nginx]
tags: [web, restart]
---

# nginx restarts under load

worker_connections is 1024 on this fleet; 5xx bursts line up with a 512 file
descriptor limit in the systemd unit rather than with traffic itself.

## Root causes

- 2026-11-03: LimitNOFILE missing from the unit drop-in
