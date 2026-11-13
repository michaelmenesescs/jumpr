---
status: resolved
tags: [api, cache, varnish, proxy, edge, ewr, 5xx, timeout]
---

# edge cache 5xx in EWR

Traffic reached a fresh varnish node before the load balancer had warmed it.
The health check passed on the admin port, which is why the node stayed in the
pool.

## Resolution

Warm from the load balancer and pin the admin-port check out of the health
check list.
