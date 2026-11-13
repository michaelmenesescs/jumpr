---
id: 2026-11-12-api-5xx-ewr
status: resolved
opened: 2026-11-12T14:02:11-05:00
window: [2026-11-12T13:30:00-05:00, now]
targets: [web-ewr-001.example.internal, web-ewr-002.example.internal, web-ewr-003.example.internal, web-ewr-004.example.internal, web-ewr-005.example.internal, web-ewr-006.example.internal, web-ewr-007.example.internal, web-ewr-008.example.internal, web-ewr-009.example.internal, web-ewr-010.example.internal, web-ewr-011.example.internal, web-ewr-012.example.internal, web-ewr-013.example.internal, web-ewr-014.example.internal, web-ewr-015.example.internal, web-ewr-016.example.internal, web-ewr-017.example.internal, web-ewr-018.example.internal, web-ewr-019.example.internal, web-ewr-020.example.internal, web-ewr-021.example.internal, web-ewr-022.example.internal, web-ewr-023.example.internal, web-ewr-024.example.internal]
alerts: ["HighErrorRate{service=\"api\"}"]
tags: [api, nginx]
resolved: 2026-11-12T14:08:00-05:00
---
# API 5xx in EWR

## Timeline
- 14:02:11 **open** from alert `HighErrorRate` (24 hosts)
- 14:02:40 **probe** `failed-units` on 24 hosts → 22 identical, 2 differ (`nginx.service failed`)
- 14:03:05 **journal** nginx on web-ewr-007 → `bind() to 0.0.0.0:443 failed (98: Address already in use)`
  <details><summary>output (38 lines)</summary>

  ```
  Nov 12 14:03:01 web-ewr-007 nginx[1001]: worker 1 started
  Nov 12 14:03:02 web-ewr-007 nginx[1002]: worker 2 started
  Nov 12 14:03:03 web-ewr-007 nginx[1003]: worker 3 started
  Nov 12 14:03:04 web-ewr-007 nginx[1004]: worker 4 started
  Nov 12 14:03:05 web-ewr-007 nginx[1005]: worker 5 started
  Nov 12 14:03:06 web-ewr-007 nginx[1006]: worker 6 started
  Nov 12 14:03:07 web-ewr-007 nginx[1007]: worker 7 started
  Nov 12 14:03:08 web-ewr-007 nginx[1008]: worker 8 started
  Nov 12 14:03:09 web-ewr-007 nginx[1009]: worker 9 started
  Nov 12 14:03:00 web-ewr-007 nginx[1010]: worker 10 started
  Nov 12 14:03:01 web-ewr-007 nginx[1011]: worker 11 started
  Nov 12 14:03:02 web-ewr-007 nginx[1012]: worker 12 started
  Nov 12 14:03:03 web-ewr-007 nginx[1013]: worker 13 started
  Nov 12 14:03:04 web-ewr-007 nginx[1014]: worker 14 started
  Nov 12 14:03:05 web-ewr-007 nginx[1015]: worker 15 started
  Nov 12 14:03:06 web-ewr-007 nginx[1016]: worker 16 started
  Nov 12 14:03:07 web-ewr-007 nginx[1017]: worker 17 started
  Nov 12 14:03:08 web-ewr-007 nginx[1018]: worker 18 started
  Nov 12 14:03:09 web-ewr-007 nginx[1019]: worker 19 started
  Nov 12 14:03:00 web-ewr-007 nginx[1020]: worker 20 started
  Nov 12 14:03:01 web-ewr-007 nginx[1021]: worker 21 started
  Nov 12 14:03:02 web-ewr-007 nginx[1022]: worker 22 started
  Nov 12 14:03:03 web-ewr-007 nginx[1023]: worker 23 started
  Nov 12 14:03:04 web-ewr-007 nginx[1024]: worker 24 started
  Nov 12 14:03:05 web-ewr-007 nginx[1025]: worker 25 started
  Nov 12 14:03:06 web-ewr-007 nginx[1026]: worker 26 started
  Nov 12 14:03:07 web-ewr-007 nginx[1027]: worker 27 started
  Nov 12 14:03:08 web-ewr-007 nginx[1028]: worker 28 started
  Nov 12 14:03:09 web-ewr-007 nginx[1029]: worker 29 started
  Nov 12 14:03:00 web-ewr-007 nginx[1030]: worker 30 started
  Nov 12 14:03:01 web-ewr-007 nginx[1031]: worker 31 started
  Nov 12 14:03:02 web-ewr-007 nginx[1032]: worker 32 started
  Nov 12 14:03:03 web-ewr-007 nginx[1033]: worker 33 started
  Nov 12 14:03:04 web-ewr-007 nginx[1034]: worker 34 started
  Nov 12 14:03:05 web-ewr-007 nginx[1035]: worker 35 started
  Nov 12 14:03:06 web-ewr-007 nginx[1036]: worker 36 started
  Nov 12 14:03:07 web-ewr-007 nginx[1037]: worker 37 started
  Nov 12 14:03:08 web-ewr-007 nginx[1038]: worker 38 started
  ```
  </details>
- 14:05:12 **action** `systemctl restart nginx` on 2 hosts → ok 2/2
- 14:06:00 **note** stale haproxy bound :443 after salt run 20261112135501
- 14:07:00 **finding** haproxy held :443, so nginx never bound

## Findings
- haproxy held :443, so nginx never bound

## Resolution
stale haproxy process bound :443; restarted haproxy
