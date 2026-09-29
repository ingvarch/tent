# tent-node-upload

A development build of tent is any build that is not an exact release, such as `make build` output. No release holds
its tent-node, so tent reads where its nodes download tent-node from two variables: `TENT_NODE_URL` and
`TENT_NODE_SHA256`. `tent-node-upload` sets them up:

1. It checks that the binary is a 64-bit linux ELF executable for `-arch` and computes its sha256.
2. It looks for `<prefix>/tent-node/<sha256>/tent-node_linux_<arch>` in the bucket with a HEAD request, and uploads
   the binary there when the object is missing. It also uploads it again when the lifecycle rule would delete the
   object before the new URL expires.
3. It presigns a GET of the object for `-expires` (7 days by default, at most 7 days) and prints two lines that set
   `TENT_NODE_URL` and `TENT_NODE_SHA256`, for fish or for sh. Everything else goes to stderr.

Dev builds live in the CI R2 bucket under `dev/` (maintainer decision 18).

## Setup (once)

- **An R2 API token for your machine**, separate from the CI token. In the Cloudflare dashboard: R2 Object Storage →
  Manage API tokens → Create API token, with the permission **Object Read & Write**, applied to the CI bucket only.
  R2 cannot limit a token to a prefix, so the token can also write the bucket's `ci/` objects. Keep its Access Key ID
  and Secret Access Key in your password manager. Never commit them or paste them into a chat.
- **A lifecycle rule** on the bucket: R2 Object Storage → the bucket → Settings → Object lifecycle rules → Add rule.
  Apply it to the prefix `dev/` and delete objects **8 days** after upload. The tool assumes these 8 days: a URL works
  for at most 7, and an object older than a day is uploaded again when a new 7-day URL needs it.

## Use

The tool reads:

| Variable | Value |
|---|---|
| `TENT_DEV_S3_URL` | `s3://<bucket>/dev?endpoint=https://<account id>.r2.cloudflarestorage.com&region=auto`, the form of tent's s3 state store URL (`pathStyle=true` only for servers without bucket host names) |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | the R2 token's keys; or `AWS_PROFILE` with a profile in `~/.aws/credentials` |

In fish:

```fish
set -gx TENT_DEV_S3_URL 's3://<bucket>/dev?endpoint=https://<account id>.r2.cloudflarestorage.com&region=auto'
# From your password manager: typed on the command line, the keys would stay in fish's history.
set -x AWS_ACCESS_KEY_ID (<password manager command>)
set -x AWS_SECRET_ACCESS_KEY (<password manager command>)

# Build tent and tent-node for linux/amd64, upload tent-node, and set the two variables in this shell.
make dev-upload | source

# bin/tent from the same build now gives its nodes this tent-node.
./bin/tent update cluster <name> --yes
```

Run `make dev-upload` from the repository root, not with `make -C`: GNU make 4 then prints "Entering directory" lines
to stdout, which the shell would run.

`make dev-upload` prints fish lines when `$SHELL` is fish and sh lines otherwise. Only those two lines go to stdout;
the build's commands and output, the upload's progress and any error go to stderr. After a failure nothing reaches
stdout, so the shell keeps the `TENT_NODE_URL` and `TENT_NODE_SHA256` it had: read stderr before you use them.

In sh or bash: `eval "$(make dev-upload)"`. `$SHELL` names your login shell, so in bash started from a fish login,
ask for sh lines: `eval "$(env SHELL=/bin/sh make dev-upload)"`.

Run the tool directly for another binary, architecture or expiry:

```fish
go run ./hack/tent-node-upload -binary bin/tent-node_linux_arm64 -arch arm64 -expires 24h -shell fish | source
```

Flags: `-binary` (default `bin/tent-node_linux_<arch>`), `-arch` (`amd64` or `arm64`, default `amd64`), `-expires`
(from `1s` to `168h`, default `168h`), `-shell` (`fish` or `sh`, default by `$SHELL`). `-h` lists them.

## Notes

- **amd64 only.** `make build` builds tent-node for linux/amd64 alone, and a development build of tent gives every
  node that one URL, whatever its architecture. Its clusters need amd64 plans: on an arm64 plan the node fails at
  cloud-init with an exec format error.
- **Every build is a new object.** `make build` stamps the build time into the binary, so each `make dev-upload`
  uploads a new object. The lifecycle rule deletes them.
- **The URL grants a download.** It carries the token's Access Key ID and a signature, never the secret key. Anyone
  who has the URL can download that one object until the URL expires. tent writes it into the nodes' user data. Do not
  post it anywhere.
- **Temporary credentials.** With a session token (`AWS_SESSION_TOKEN`, SSO, an assumed role) the URL carries the
  token and stops working when the session ends. The tool warns when the credentials expire before the URL, or carry a
  token without an expiry. The static keys of an R2 token do not expire.
