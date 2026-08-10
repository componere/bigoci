---
title: Authenticate to a registry
description: Give bigoci the credentials a private OCI registry asks for, and read the failures when it refuses.
---

# Authenticate to a registry

This guide gives bigoci a credential for a registry that will not serve you
without one. It assumes you can already push and pull against a registry that
needs no credential — see [Push and pull a file](push-and-pull.md).

Nothing here is needed for public artifacts. A registry that never asks for a
credential behaves exactly as it did before. A registry that hands out tokens
for public reads still gets that token exchange, with no credential in it.

## Use the credentials you logged in with

Most setups already have a credential on disk. Log in once with Docker:

```sh
docker login ghcr.io
```

Then tell bigoci to use what that wrote:

```go
client, err := bigoci.New(bigoci.WithDockerCredentials())
if err != nil {
    return err
}
```

bigoci reads `$DOCKER_CONFIG/config.json` when that variable is set, and
`~/.docker/config.json` otherwise (`%USERPROFILE%\.docker\config.json` on
Windows). It looks the registry up under the host your reference names, so a
login to `ghcr.io` serves a push to `ghcr.io/you/model:v1` and nothing else.

Two things to know:

- The file is read when you build the client. A malformed file fails there,
  which is the one error `bigoci.New` returns today. A file that is not there
  — or a machine with no home directory to look in — is not an error: you get
  anonymous behavior.
- bigoci only reads. No transfer writes a credential anywhere.

### Credential helpers run as programs

If your configuration names a credential helper — `"credsStore": "osxkeychain"`,
or a `credHelpers` entry for one registry — then resolving a credential means
**running that program**. bigoci executes `docker-credential-<name>` from your
`PATH`, hands it the registry name, and reads what it prints, which is the same
thing the Docker command line does. Cloud helpers such as
`docker-credential-ecr-login` work for that reason and need no bigoci-side code.

bigoci reads no configuration and runs no helper until you name
[`WithDockerCredentials`](https://pkg.go.dev/github.com/componere/bigoci#WithDockerCredentials).

A helper that hangs does not hang your transfer: a lookup gives up after ten
seconds and fails the transfer with what the helper was doing.

One caveat comes with running someone else's program: whatever a helper writes
to its own standard error goes straight to yours, and bigoci cannot redact it.
A helper that prints a credential while failing puts it in front of whoever is
watching the terminal or the CI log.

## Pass a credential directly

In CI you usually hold the secret already, in an environment variable. Skip the
file:

```go
client, err := bigoci.New(bigoci.WithCredentials(os.Getenv("REGISTRY_USER"), os.Getenv("REGISTRY_TOKEN")))
```

The second argument is a password or, at most registries today, a personal
access token. This credential goes to whatever registry your reference names —
you chose both — so keep the reference and the secret in the same hand.

## The command line always uses your login

The reference CLI needs no flag and has none:

```sh
docker login ghcr.io
bigoci push /data/model.bin ghcr.io/you/model:v1
```

To run it with no credential, point it at an empty configuration directory:

```sh
DOCKER_CONFIG=$(mktemp -d) bigoci pull ghcr.io/you/model:v1 /data/model.bin
```

## Read a refusal

A refused transfer comes back as `bigoci.ErrUnauthorized`, which the CLI reports
as exit code 6:

```go
if errors.Is(err, bigoci.ErrUnauthorized) {
    // log in, or ask for access
}
```

It has three causes, and the error message says which:

1. **You presented nothing and the registry wants something.** Log in to that
   registry, and name a credential option if you are using the library.
2. **You presented something and the registry said no.** The password or token
   is wrong or expired, or the account it belongs to may not reach that
   repository. A push needs write access where a pull needs read, so a
   credential that pulls fine can still fail a push — the message names the
   access that was refused.
3. **The refusal was not about credentials.** A proxy or a web application
   firewall in front of the registry answers 403 too, and bigoci cannot tell
   that apart from a permission answer. If logging in and checking access both
   come up clean, look at what sits in front of the registry.

bigoci does not retry a refusal. Presenting the same credential again gives
the same answer, so a wrong password costs no waiting and only the requests
the protocol itself spends: the request the registry challenged, one token
exchange where the registry uses one, and the single re-issue that challenge
bought. The one exception is invisible: a token that expires while a transfer
is running is replaced and the transfer carries on.

## Limitations

**Identity tokens are refused, not used.** Some logins — Azure Container
Registry with `az acr login`, and a few others — store an OAuth2 refresh token
in the configuration instead of a password, under `identitytoken`. bigoci cannot
exchange one and says so rather than quietly transferring anonymously. Log in
again with a password or an access token, which for ACR means an admin account
or a service principal.

## If you need a credential source bigoci does not have

The escape hatch is your own transport, through
[`WithHTTPClient`](https://pkg.go.dev/github.com/componere/bigoci#WithHTTPClient).
go-containerregistry's `authn` keychain is the usual reason: it resolves ECR,
ACR, and Artifact Registry credentials in process, with no helper binaries.

**A `RoundTripper` that adds `Authorization` to every request will send your
credential to hosts that are not the registry.** bigoci does not talk only to
the registry: the token exchange goes to whatever host the registry's challenge
names, and large registries answer a blob read with a redirect to object
storage, whose URL is already signed. A credential that arrives there is a
credential in somebody else's logs, and the request would have worked without
it.

The fix is a host check — add the header only for the registry you built the
transport for:

```go
func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
    if req.URL.Host != t.registry {
        return t.base.RoundTrip(req) // never off the registry
    }

    req = req.Clone(req.Context())
    req.Header.Set("Authorization", t.header)

    return t.base.RoundTrip(req)
}
```

Compare the host, not the domain: `cdn.example.com` is not
`registry.example.com`, however closely they are related.
