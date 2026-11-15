# Chef fixtures

The shapes the Chef parsers are written against:

status.json: knife status -F json
node-list.json: knife node list -F json
node-show.json: knife node show web-01 -l -F json
role-list.json: knife role list -F json
env-list.json: knife environment list -F json
search.json: knife search node '*:*' -a platform -F json

The lab capture that refreshes these files is:

    go run ./cmd/sitrep-lab capture chef --config-dir lab/jumpr

That run rewrites every file here from the lab's chef-zero
(`chef.lab.example.internal`, knife configured for user `sre`) and regenerates
this README with the exact command per file. The files committed with task 113
were written by hand from knife's own JSON shapes, because the capture needs the
lab up; re-run the capture with the lab running before trusting them as real
output. `internal/backend/chef`'s tests read them from here.

status.json's `ohai_time` values are fixed epoch seconds chosen against
2026-11-13T12:00:00Z, the clock the Chef tests run on: web-01 is 5 minutes old,
web-02 3 hours, db-01 30 hours, and web-03 carries no ohai_time at all.
