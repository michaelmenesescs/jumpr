---
alerts: [api-5xx]
tags: [api]
---

# api-5xx alert

The alert fires on a 5 minute rate, twice. In EWR the usual cause is the edge
cache failing warm-up, not the application.

## Root causes

- 2026-11-01: varnish warm-up missed the new build
