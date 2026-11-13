# auth-helper-cookie — example jumper auth helper

An auth helper is any executable jumper runs for a `type: helper` connection
profile. It prints JSON on stdout and jumper keeps the result in memory only:

```json
{"headers": {"Authorization": "Bearer …"},
 "cookies": [{"name": "_oauth2_proxy", "value": "…", "domain": ".example.internal"}],
 "expires_at": "2026-11-12T18:00:00Z"}
```

This example returns one cookie for an SSO proxy. The value comes from
`JUMPER_AUTH_COOKIE`, or from the macOS Keychain when that is unset.

## Use it

```yaml
# ~/.config/jumper/connections.yaml
connections:
  corp-sso:
    auth:
      type: helper
      helper:
        command: ["~/go/bin/auth-helper-cookie", "-name", "_oauth2_proxy", "-domain", ".example.internal"]
        cache_ttl: 50m
        refresh_on: [401, 403, 302-to-login]
```

Store the cookie once in the Keychain (the value is never written to a file):

```
security add-generic-password -s jumper-auth -a _oauth2_proxy -w
```

`-expires` (default `50m`) sets `expires_at`, after which jumper runs the helper
again. `refresh_on` lists the statuses and login-redirect patterns that count as
"the session went stale": jumper then invalidates its cache, runs the helper
once, retries the request once, and otherwise reports an `auth` error naming the
profile.

Build and install it with:

```
go build -o ~/go/bin/auth-helper-cookie ./examples/auth-helper-cookie
```
