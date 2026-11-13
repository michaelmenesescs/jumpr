---
hosts: [edge-ewr-001.example.internal]
tags: [cache, varnish]
---

# edge cache warm-up

A cold varnish instance answers 503 for a few seconds after a restart. Warm it
from the load balancer before flipping traffic.

This paragraph is not the snippet.
