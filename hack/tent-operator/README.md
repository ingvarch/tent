# tent-operator

Gives the operator access to the Nomad API of a cluster that tent built. It is temporary: `tent export nomad` (M2.8)
will do this job, and this tool goes away with it.

A cluster's Nomad API takes mTLS and an ACL token. tent keeps the cluster's CA and the ACL bootstrap secret in the
state store. `tent-operator`:

1. Reads the completed spec (for the Nomad region), the CA key, the CA bundle and the bootstrap secret from the state
   store, the way the tent CLI opens it: `file:///abs/path` or `s3://bucket[/prefix]?...`.
2. Issues an operator certificate, `cli.<region>.nomad`, for client authentication, valid for `-ttl` (1 hour by
   default).
3. Makes the new directory `-dir` (mode 0700) and writes `ca.pem`, `cli.pem`, `cli-key.pem` and `token` into it (mode
   0600). It never overwrites: a directory that exists is an error.
4. Prints to stdout the lines that set `NOMAD_ADDR` (from `-addr`), `NOMAD_CACERT`, `NOMAD_CLIENT_CERT`,
   `NOMAD_CLIENT_KEY` and `NOMAD_TLS_SERVER_NAME` (`server.<region>.nomad`), for fish or sh.
5. Prints to stderr the line that sets `NOMAD_TOKEN` from the token file. The secret never goes to the terminal, and
   the CA key never leaves the tool.

The cluster must have run `tent update cluster --yes` once, so that the state store holds the CA and the secret. The
tool reads the store only; it needs no cloud credentials. `tent` prints private addresses, so take the public address
of a server from the cloud's console or API.

## Use

In fish:

```fish
set -gx TENT_STATE 'file:///abs/path/to/state'
set -gx TENT_CLUSTER prod

go run ./hack/tent-operator -addr https://203.0.113.5:4646 -dir ~/.cache/tent-prod | source
set -gx NOMAD_TOKEN (cat ~/.cache/tent-prod/token)   # the line that stderr printed

nomad server members
```

In sh or bash: `eval "$(env SHELL=/bin/sh go run ./hack/tent-operator ...)"`.

Flags: `-state` (default `$TENT_STATE`), `-name` (default `$TENT_CLUSTER`), `-dir` (required; a relative path
is made absolute, so the printed paths work after a `cd`), `-addr` (required, an https address), `-ttl` (default
`1h`), `-shell` (`fish` or `sh`, default by `$SHELL`). `-h` lists them.

## Notes

- **Remove the directory when done.** It holds the operator's key and the cluster's ACL bootstrap secret, a token
  that can do anything in the cluster.
- **A new run needs a new directory.** The certificate ends after `-ttl`; run the tool again with another `-dir`.
- **The bootstrap secret is the management token** until tent issues other tokens.
