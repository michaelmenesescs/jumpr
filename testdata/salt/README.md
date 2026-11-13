# Salt golden files

Captured from the lab's Salt master (`salt.lab.example.internal`) by
`go run ./cmd/sitrep-lab capture salt --config-dir lab/jumper`.

Every file is the Salt CLI's own output, passed through `devtools.Sanitize`.
`pillar-items.json` is additionally passed through `devtools.ScrubValues`, so
every leaf value is `REDACTED-<type>` and no pillar value is ever recorded.

One line per file: the exact command that produced it.

version.txt: salt --version
key-list.json: salt-key --out=json --out-indent=-1 --no-color -L
key-finger.json: salt-key --out=json --out-indent=-1 --no-color -F
manage-status.json: salt-run --out=json --out-indent=-1 --no-color manage.status
manage-versions.json: salt-run --out=json --out-indent=-1 --no-color manage.versions
cache-grains.json: salt-run --out=json --out-indent=-1 --no-color cache.grains 'tgt=*'
nodegroups.json: salt-run --out=json --out-indent=-1 --no-color config.get nodegroups
preview-compound.txt: salt -C G@role:web --preview-target
preview-compound.json: salt --out=json --out-indent=-1 --no-color --preview-target -C G@role:web
async-submit.txt: salt --out=json --out-indent=-1 --no-color --async '*' test.ping
list-job-ping.json: salt-run --out=json --out-indent=-1 --no-color jobs.list_job '<jid>'
jobs-active.json: salt-run --out=json --out-indent=-1 --no-color jobs.active
jobs-list.json: salt-run --out=json --out-indent=-1 --no-color jobs.list_jobs
cmd-run-all.json: salt-run --out=json --out-indent=-1 --no-color jobs.list_job '<jid>'
state-test-clean.json: salt-run --out=json --out-indent=-1 --no-color jobs.list_job '<jid>'
state-test-changes.json: salt-run --out=json --out-indent=-1 --no-color jobs.list_job '<jid>'
state-render-error.json: salt --out=json --out-indent=-1 --no-color -L web-01 state.sls nosuch test=True
show-top.json: salt-run --out=json --out-indent=-1 --no-color state.show_top
show-sls-nginx.json: salt-run --out=json --out-indent=-1 --no-color state.show_sls nginx
show-highstate.json: salt --out=json --out-indent=-1 --no-color -L web-01 state.show_highstate
pillar-items.json: salt --out=json --out-indent=-1 --no-color -L web-01 pillar.items
mine.json: salt-run --out=json --out-indent=-1 --no-color mine.get '*' network.ip_addrs
fileserver-envs.json: salt-run --out=json --out-indent=-1 --no-color fileserver.envs
fileserver-file-list.json: salt-run --out=json --out-indent=-1 --no-color fileserver.file_list saltenv=base
event-sample.txt: (sleep 1; salt --out=json --no-color '*' test.ping >/dev/null 2>&1) & timeout 10 salt-run state.event pretty=False
no-return.json: salt-run --out=json --out-indent=-1 --no-color jobs.list_job '<jid>'

## Deviations

- Nothing has been captured yet: the lab was not running when task 067 was committed (`orb list` reports
  that the OrbStack socket is not connectible). Run the command above with the lab up; it rewrites this
  file and adds a line here for every command that fails or whose output shape is unexpected.
- Until then, tasks 068-075 must treat their inline samples as provisional and follow these files once
  they exist.
