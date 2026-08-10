# bigoci reference CLI

**This is not a product. It is never published, never released, and never
versioned.** There is no compatibility promise of any kind. It exists so a
human can watch the bigoci library work against a real registry. Nothing in
this repository publishes it, no release job builds it, and the `replace`
directive in its `go.mod` points at the working tree, which makes
`go install github.com/componere/bigoci/cli@latest` impossible on purpose.

If you are looking for a supported way to move large files to a registry, use
the library.

## Why it exists

The library ships no command line tool by design. But some of what the library
claims is only convincing when you watch it happen against a registry that
nobody wrote for the occasion: that a re-push of an unchanged file sends no blob
bytes, that an interrupted pull leaves a partial file behind, that no credential
ends up in a log. This CLI's own tests check the first of those against a fake
registry, which proves the wiring; a real one is what proves the claim. This is
what a person watches it with.

Concretely, it is the instrument for the manual gates of the library's
implementation phases 2 through 5. Section [Reading the output](#reading-the-output)
is the cookbook for those gates.

It is thin by rule:

- every flag maps onto exactly one public library option, or onto one standard
  library call;
- it contains no transfer logic, no knowledge of the artifact format, and no
  retry, resume, or authentication logic;
- it defines no interfaces.

If a change here would break one of those rules, the library is missing
something and the fix belongs there.

## Build and run

```
cd cli
go build -o bin/bigoci .
./bin/bigoci help
```

That builds the library from the working tree next door, through the `replace`
directive. It never builds a published version, because there is none.

## Usage

```
bigoci push [flags] <file> <ref>
bigoci pull [flags] <ref> <dest>
bigoci help [push|pull]
```

`bigoci` with no arguments, `-h`, and `--help` all print the same usage text as
`bigoci help`.

`<ref>` is passed to the library exactly as typed. The CLI parses no reference
grammar, so whatever the library accepts is what works and whatever it refuses
is refused in the library's own words.

There is no `version` command and no `-version` flag. A version string on a
tool that is never released invites exactly the wrong reading. Build provenance
is in the log instead: the first `-debug` line records the commit and the Go
version the binary was built from.

### Flags

Each flag maps onto one library option. The mapping is the whole point of the
table, so it is easy to check that the CLI adds nothing of its own.

| Flag | Applies to | Maps to |
|---|---|---|
| `-part-size <size>` | push | `bigoci.WithPartSize` |
| `-title <string>` | push | `bigoci.WithTitle` |
| `-workers <int>` | both | `bigoci.WithWorkers` |
| `-plain-http` | both | `bigoci.WithPlainHTTP` |
| `-debug` | both | `bigoci.WithHTTPClient` with an observing transport |
| `-timeout <duration>` | both | `context.WithTimeout` around the call |

### Unset means absent

A flag you do not set contributes nothing. The CLI never restates a library
default, so the library's own default applies, and re-measuring one changes what
the CLI does with no change here.

Two consequences worth stating outright:

- `-title ""` and no `-title` are different. `-title ""` passes
  `WithTitle("")`, which writes no file name annotation. Omitting `-title`
  passes nothing, and the library names the artifact after the base name of the
  file it read.
- Help text shows the library's real defaults, read at runtime:
  `-workers int` says `(unset: the library default, 8)`. The registered default
  is the zero value, which is why no `(default 0)` appears beside it.

### Why pull has no -part-size and no -title

Both describe how a push chose to store a file, and both travel in the
manifest. A pull reads them from there and cannot be told otherwise. Asking for
`-part-size` on a pull is an error, from the standard library's own flag
parsing:

```
$ bigoci pull -part-size 4MiB reg/repo:v1 out.bin
bigoci: pull: flag provided but not defined: -part-size
```

### Part size grammar

```
size   := digits unit?
digits := one or more ASCII decimal digits
unit   := B | K | KiB | M | MiB | G | GiB      (case-insensitive)
```

No unit, or `B`, means bytes. `K`/`KiB` is 1024, `M`/`MiB` is 1048576,
`G`/`GiB` is 1073741824. Binary units only.

The grammar tops out at `GiB`. There is no `TiB` and no larger unit: a part that
big has no realistic use, so it is an unknown unit like any other typo rather
than a special case with a story.

Refused, with a message that says what to write instead: spaces anywhere,
fractions, a sign, a zero size, an unknown unit, and a value too large for a
byte count.

Decimal SI units are refused on purpose:

```
$ bigoci push -part-size 4MB model.bin reg/repo:v1
bigoci: push: invalid value "4MB" for flag -part-size: part size "4MB": decimal SI
units are not supported; write 4MiB (1 MiB = 1048576 bytes) or a plain byte count
```

`4MB` meaning 4000000 in one tool and 4194304 in another is not a rounding
difference here. The part size is part of what the manifest digest describes, so
a unit that quietly means something else produces a different artifact.

### Flags come before the operands

The standard library's flag parsing stops at the first operand, so a flag
written after one would silently become an operand. On an instrument whose job
is to show what happened, silently ignoring `-debug` is not acceptable:

```
$ bigoci push model.bin reg/repo:v1 -debug
bigoci: push: flags must come before the operands; move "-debug" before "model.bin"
```

That is exit 2, with push's usage block under it. No operand this CLI takes — a
file path, a reference, a destination path — begins with a dash unless you say
so with `--`.

`--` ends the flags. Everything after it is an operand whatever it looks like,
which is how you name a file whose name really does begin with a dash:

```
bigoci push -plain-http -- -weird-name.bin 127.0.0.1:5050/team/model:v1
```

Without the `--`, the flag parser reaches that name first and refuses it as a
flag nobody declared:

```
$ bigoci push -weird-name.bin 127.0.0.1:5050/team/model:v1
bigoci: push: flag provided but not defined: -weird-name.bin
```

Both refusals are exit 2, and both print the offending command's usage block.

There are no `-` operands for standard input or standard output, and there
cannot be: a push re-reads each part from the file to upload it, so the source
has to be seekable.

### -timeout

`-timeout` bounds the whole transfer. Leaving it unset adds no deadline at all,
rather than a very long one. It bounds the library's retries and the waits
between them too: a deadline that expires while a worker is backing off ends
that wait immediately rather than after it.

An explicit `-timeout 0` means the same thing: no limit. A negative duration is
a usage error, exit 2, because someone who typed a sign by mistake should hear
about it rather than watch a transfer run unbounded:

```
$ bigoci push -timeout -30s model.bin 127.0.0.1:5050/team/model:v1
bigoci: push: -timeout must not be negative, got -30s
```

A deadline that expires is exit 1, not 2. It says so on the first failure line
and still reports the failure underneath it.

## Output contract

This is a contract, not a description. Recipes read these streams.

**Standard output is data only.**

- A push writes exactly one line: the digest of the manifest it wrote. Nothing
  else, ever. On failure it writes nothing at all, so
  `d=$(bigoci push model.bin reg/repo:v1)` either sets `d` to a digest or
  leaves it empty.
- A pull writes nothing, whether it succeeds or fails.
- Help asked for by name goes to standard output, and exits zero.

**Standard error is everything else.** Every line is prefixed `bigoci: `,
except request log lines, which are prefixed `http> `, `http< `, and `http! `.

```
bigoci: push /data/model.bin (204800 bytes) -> 127.0.0.1:5050/team/model:v1 (part-size=64KiB, workers=8, plain-http)
bigoci: pushed sha256:829c96af3ccd… in 12.4s

bigoci: pull 127.0.0.1:5050/team/model:v1 -> /data/out.bin (workers=8, plain-http)
bigoci: pulled 204800 bytes in 9.1s
```

The preflight line reports the values the transfer will really run with: the
flags where they were set, the library's own defaults where they were not.
`plain-http` appears only when the flag is set. The push line's byte count comes
from a stat of the file; if that fails the whole line is left out, because the
library is the one that reports an unreadable file. A pull's byte count is read
back from the file it published.

There is no terminal detection, no color, no progress bar, and no line
rewriting. The output is byte-identical piped and interactive. Progress, when
it arrives, will arrive as whole lines.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | failure, no sentinel matched (transport, local file, malformed reference, timeout) |
| 2 | usage error |
| 3 | `errors.Is(err, bigoci.ErrNotFound)` |
| 4 | `errors.Is(err, bigoci.ErrNotBigociArtifact)` |
| 5 | `errors.Is(err, bigoci.ErrDigestMismatch)` |
| 6 | `errors.Is(err, bigoci.ErrUnauthorized)` |
| 7 | `errors.Is(err, bigoci.ErrPartTooLarge)` |
| 130 | interrupted by SIGINT |
| 143 | terminated by SIGTERM |

A failure always prints two lines:

```
bigoci: not found: pull 127.0.0.1:5050/team/model:absent to /data/x.bin: fetch the
manifest: GET /v2/team/model/manifests/absent: registry returned 404 Not Found: …
bigoci: matched sentinel bigoci.ErrNotFound (exit 3)
```

The first line is the library's error verbatim, never re-wrapped and never
re-phrased. The second line is unconditional, because it is how a shell script
watches the library's error classification work from outside Go. It takes one of
three forms:

| Second line | When |
|---|---|
| `bigoci: matched sentinel <name> (exit <code>)` | `errors.Is` matched a sentinel |
| `bigoci: no sentinel matched (exit 1)` | none did |
| `bigoci: interrupted by SIGINT (exit 130)` or `bigoci: terminated by SIGTERM (exit 143)` | a signal stopped the run |

A usage error prints its complaint and then a usage block instead of a second
line, and exits 2.

A deadline that expired says so first, and still reports the failure
underneath:

```
bigoci: timed out after 1ns: pull …: fetch the manifest: … context deadline exceeded
bigoci: no sentinel matched (exit 1)
```

Two judgment calls are worth stating, because both are arguable:

- A missing or unreadable local file is exit 1, not 3. `ErrNotFound` is about
  what the registry does not hold.
- A malformed reference is exit 1 today. The library reports it as a plain
  error; whether it deserves a sentinel is feedback for the library, not
  something to paper over here.

### Interrupts

The first Ctrl-C or SIGTERM cancels the transfer and prints:

```
bigoci: interrupted (SIGINT), stopping; press Ctrl-C again to force quit
```

The handler cancels and nothing else — it never exits the process — so the
transfer unwinds on its own and a pull's `.bigoci-partial` file is left where a
later resume can find it. After the first delivery the signal goes back to its
default action, so a second Ctrl-C kills a wedged transfer.

The transfer then fails, and its two lines say a signal stopped it:

```
bigoci: pull 127.0.0.1:5050/team/model:v1 to /data/out.bin: part 3: context canceled
bigoci: interrupted by SIGINT (exit 130)
```

A recorded signal outranks the error's shape. Cancelling a transfer surfaces as
whatever the unwinding produced first — `context.Canceled`, a closed file, a
reset socket, sometimes an error that matches a sentinel — and none of that
changes why the run stopped. So once a signal has been received, the second line
reports the signal and the exit code is 130 or 143, whatever the first line says.

Exit 1 with `context.Canceled` on the first line and no signal line means
something else cancelled the transfer: a `-timeout` that expired, or a context
the caller cancelled.

## The -debug log format

**This format is a frozen contract.** Recipes and journal entries grep these
fields. Renaming one is a breaking change.

With `-debug` off, no HTTP client is passed to the library at all, so the
library's own default client is what runs. That default path is the thing worth
demonstrating, and a client installed to watch it would no longer be it.

With `-debug` on, the library is given a client whose transport observes and
forwards. The observer never changes a request, never wraps or reads a body in
either direction, never sets a timeout, and never sets a redirect policy. Sizes
come from `Content-Length` alone. No body is ever logged, in either direction,
not even truncated.

### The three line kinds

```
http> <seq> <t> <METHOD> <URL> class=<class> auth=<auth> clen=<n> type=<v> range=<v> accept=<v>
http< <seq> <t> <METHOD> <URL> class=<class> status=<code> dur=<d> clen=<n> ctype=<v> crange=<v> loc=<v> ddigest=<v> retry-after=<v> challenge=<v>
http! <seq> <t> <METHOD> <URL> class=<class> dur=<d> err=<v>
```

`http>` is written before the request leaves, so a hang or a dead port is
visible while it is happening. `http<` is written when the response headers
arrive. `http!` is written when the request never got a response.

| Field | Meaning |
|---|---|
| `<seq>` | request counter, zero-padded to four digits and growing wider past 9999; pairs an `http>` line with its `http<` or `http!` |
| `<t>` | `+%.3fs` since the log opened; a backoff gap reads straight off the page |
| `<METHOD>` | padded to four columns so the URLs line up |
| `<URL>` | the absolute URL, redacted (below) |
| `class` | what kind of request it is, inferred from the URL shape (below) |
| `auth` | exactly one of `none`, `bearer`, `basic`, `other` |
| `status` | the HTTP status code |
| `dur` | time to response headers, rounded to 100µs |
| `clen` | `Content-Length` of the request or the response; `-1` means it said none |
| `err` | the transport error, quoted, so one failure is always one line |

Every other field is a header. A header that was present is quoted; a header
that was absent is a bare `-`, so a header whose value happens to be a dash is
still distinguishable.

A `clen=-1` on a blob upload would mean a chunked `PUT`, which would be a
regression of the library's explicit Content-Length invariant. That is the kind
of thing this line exists to make visible.

### Header allow-lists

These are the only headers that are ever rendered. There is no escape hatch and
no way to add one at runtime — a header not on the list has no code path to the
log, so a private header or a cookie a later phase starts sending cannot leak
by being forgotten.

Requests: `Authorization` (scheme only), `Content-Type` as `type`, `Range` as
`range`, `Accept` as `accept`.

Responses: `Content-Type` as `ctype`, `Content-Range` as `crange`, `Location`
as `loc`, `Docker-Content-Digest` as `ddigest`, `Retry-After` as
`retry-after`, `WWW-Authenticate` as `challenge`.

### Redaction

- `auth=` is the scheme of the credential and nothing else. The credential
  itself is unrepresentable: no prefix, no length, no fingerprint.
- URLs lose their userinfo outright.
- Query parameter values are replaced with `…`. Parameter names are kept and
  sorted, so two runs of the same transfer produce the same text.
- Parameter names are escaped again on the way out. Every byte of a name was
  chosen by the peer being logged, and a name that decodes to a newline would
  otherwise forge a second log line.
- One value passes through: a `digest` whose value verifiably **is** a sha256
  digest, meaning `sha256:` and 64 lowercase hex bytes. That value is public and
  is the key that correlates a line with a blob. The check is on the value and
  never on the name, so a host that calls its signed token `digest` still sees it
  elided.
- A query Go cannot parse renders as a single `…` in place of the whole query,
  rather than as the parameters that happened to parse. A line never shows a
  shorter query than the request carried.
- Paths are printed as they stand, digests and all. Being able to grep a digest
  out of the log beats a shorter line.
- `Location` is resolved against the URL of the request that got it, then
  redacted like any other URL.
- `challenge` is kept as it came, truncated to 200 bytes plus `…`. A challenge
  names a realm and a scope and carries no secret.

### Classification

Pure URL shape, first match wins. The CLI learns nothing about the bigoci
format from it — these are the shapes of the OCI distribution API.

| Path contains | Method | class |
|---|---|---|
| `/blobs/uploads/` | POST | `upload-open` |
| `/blobs/uploads/` | PUT, PATCH | `blob-write` |
| `/blobs/` | HEAD | `blob-check` |
| `/blobs/` | GET | `blob-read` |
| `/manifests/` | GET | `manifest-read` |
| `/manifests/` | PUT | `manifest-write` |
| `/manifests/` | HEAD | `manifest-check` |
| anything else | any | `other` |

### The summary line

Printed once after the transfer, before the line that says how it ended:

```
bigoci: http requests=16 failed=0 blob-check=5 (0 hit, 5 miss) blob-write=5 upload-open=5 blob-read=0 manifest-read=0 manifest-write=1 manifest-check=0 other=0
```

**The shape is fixed.** Every class prints every time, in the order above,
whether it was used or not, and `blob-check` always carries its `(N hit, M miss)`
suffix. That is what lets a gate grep for the zero it expects: a warm push is
proved by reading `blob-write=0`, not by noticing that a field is missing.

A blob check that answers 404 is a **miss, not a failure** — it is the answer
the question asked for, and it is the request that lets a push skip an upload. A
transport error, or any other status of 400 or more, is a failure. A blob check
that failed outright is counted under `blob-check=` and under `failed=`, and
shows up in neither the hits nor the misses.

A 401 is counted the same way, which has one consequence worth stating up
front: **against a registry that asks for a credential, a healthy transfer
reports `failed>=1`.** The 401 that carries the challenge is how the protocol
starts, and this instrument counts statuses rather than reading intent. So
`failed=0` is a gate only against a registry that never challenges; against one
that does, read the exit code and the counts of the classes you care about.
Each token exchange lands in `other=`.

## Reading the output

Everything below assumes a local registry. zot is the one the library's own
end-to-end tests use:

```
docker run --rm -p 5050:5000 ghcr.io/project-zot/zot:v2.1.20
```

That publishes 5050 rather than 5000 because on macOS port 5000 is usually taken
by AirPlay Receiver. Every recipe below uses 5050.

Make a file to work with:

```
dd if=/dev/urandom of=/tmp/model.bin bs=1M count=200
```

Every push below passes `-part-size 64MiB`, which splits that 200 MiB file into
four parts. The library's own default of 512 MiB would make it a single part, and
a single part shows none of the per-part traffic these recipes are here to read.
Four parts plus the empty config blob is **five blobs**, and that is the number
every count below is built from.

### A push writes one digest and nothing else

```
d=$(./bin/bigoci push -plain-http -part-size 64MiB /tmp/model.bin 127.0.0.1:5050/team/model:v1)
echo "$d"
```

Everything a human reads went to standard error; `$d` holds one digest.

### The manifest is what the format says it is

```
curl -s -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  "http://127.0.0.1:5050/v2/team/model/manifests/$d" |
  jq '{artifactType, layers: (.layers | length), annotations}'
```

Ask for it by the digest the push printed, not by the tag. The digest names that
one manifest whatever the tag points at later, and it is what the push actually
promised you.

Check the artifact type, that the layer count is the part count — four here — and
that the annotations carry the part size and the file name. The layers are the
parts, in order.

### A round trip is byte-identical

```
./bin/bigoci pull -plain-http "127.0.0.1:5050/team/model@$d" /tmp/out.bin
shasum -a 256 /tmp/model.bin /tmp/out.bin
```

Two identical hashes. Pulling by digest also makes the library verify the
manifest it fetched against the digest asked for. There is no `-part-size` here
and there cannot be: the part size travels in the manifest.

### A warm re-push sends no blob bytes, and is deterministic

First get a cold push on the record, into an empty repository:

```
./bin/bigoci push -plain-http -part-size 64MiB -debug /tmp/model.bin \
  127.0.0.1:5050/team/cold:v1 2>cold.log
grep '^bigoci: http ' cold.log
```

```
bigoci: http requests=16 failed=0 blob-check=5 (0 hit, 5 miss) blob-write=5 upload-open=5 blob-read=0 manifest-read=0 manifest-write=1 manifest-check=0 other=0
```

Sixteen requests: five checks that all miss, five upload sessions opened, five
blobs written, one manifest. Now re-push the same file to the repository that
already holds it:

```
d2=$(./bin/bigoci push -plain-http -part-size 64MiB -debug /tmp/model.bin \
  127.0.0.1:5050/team/model:v1 2>warm.log)
[ "$d" = "$d2" ] && echo "same digest"
grep '^bigoci: http ' warm.log
```

```
bigoci: http requests=6 failed=0 blob-check=5 (5 hit, 0 miss) blob-write=0 upload-open=0 blob-read=0 manifest-read=0 manifest-write=1 manifest-check=0 other=0
```

Six requests: five checks that all hit, `blob-write=0`, `upload-open=0`, and the
manifest written again. That one line is the HEAD-skip gate. The two identical
digests are the determinism gate: the digest is a pure function of the bytes, the
part size, and the title, so anything bound to it survives a re-push.

### A small file is one part

```
dd if=/dev/urandom of=/tmp/small.bin bs=1k count=4
s=$(./bin/bigoci push -plain-http -debug /tmp/small.bin 127.0.0.1:5050/team/small:v1)
curl -s -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  "http://127.0.0.1:5050/v2/team/small/manifests/$s" |
  jq -r '.layers | length, .[0].digest'
shasum -a 256 /tmp/small.bin
```

The manifest is the definitive check: one layer, and that layer's digest equal to
the `shasum` of the whole file, because a single part *is* the file.

The summary line is not the proof. It reads `blob-write=2` on a cold push, and
the second write is the empty config blob every OCI manifest needs — so the part
count is `blob-write - 1`, and only on a cold push, since a warm one writes no
blobs at all.

### A dead port fails fast and says where

```
./bin/bigoci push -plain-http -part-size 64MiB -debug /tmp/model.bin \
  127.0.0.1:5999/team/model:v1
echo "exit=$?"
```

Every request that goes out gets an `http!` line with `err="dial tcp …:
connect: connection refused"`. A refused connection is worth another attempt,
so the digest whose worker runs out of attempts first shows exactly four
`http!` lines. The other digests show fewer — between one and four — because
that first exhaustion cancels the peers mid-backoff, which is itself the
behavior worth seeing: nobody waits out a schedule for a transfer that is
already over. The total is at most four per worker (sixteen here), eight to
thirteen on a typical run. Then `failed=` matching whatever was sent, then
exit 1 with `no sentinel matched`.

The first failure line says how many attempts it took:

```
bigoci: push /tmp/model.bin to 127.0.0.1:5999/team/model:v1: after 4 attempts: check
whether part 0 (sha256:…) exists: HEAD /v2/team/model/blobs/sha256:…: dial tcp
127.0.0.1:5999: connect: connection refused
```

It arrives in well under ten seconds: the whole backoff budget for one unit of
work is under seven, and a refused connection on loopback comes back at once.
No `-timeout` is needed to bound the run, and leaving it off is the point —
the library is what stops, not the deadline.

The failed requests here are blob checks, so they are counted under
`blob-check=` and under `failed=`, with `(0 hit, 0 miss)`: a request that never
got an answer is neither.

### A broken connection is retried, and you can see it

Put something between the client and the registry that breaks connections, and
read the log for the same digest twice. toxiproxy is what the library's own
end-to-end tests use:

```
docker network create bigoci-gate
docker run -d --rm --network bigoci-gate --name zot ghcr.io/project-zot/zot:v2.1.20
docker run -d --rm --network bigoci-gate --name toxi -p 8474:8474 -p 8666:8666 \
  ghcr.io/shopify/toxiproxy:2.12.0 -host=0.0.0.0
curl -s -XPOST localhost:8474/proxies \
  -d '{"name":"zot","listen":"0.0.0.0:8666","upstream":"zot:5000"}'
curl -s -XPOST localhost:8474/proxies/zot/toxics \
  -d '{"name":"cut","type":"limit_data","stream":"upstream","toxicity":0.3,"attributes":{"bytes":2000000}}'

./bin/bigoci push -plain-http -part-size 16MiB -debug /tmp/model.bin \
  127.0.0.1:8666/gate/model:v1 2>push.log
echo "exit=$?"
```

A retried request is a fresh request: it gets a new `http>` line with a **new**
`seq` and the same URL, never a second line under the old one. So the evidence
is a digest that shows up more than once:

```
# part digests that were uploaded more than once
grep 'http> .*class=blob-write' push.log \
  | grep -o 'digest=sha256:[0-9a-f]\{64\}' | sort | uniq -d

# part digests that were checked more than once
grep 'http> .*class=blob-check' push.log \
  | grep -o 'blobs/sha256:[0-9a-f]\{64\}' | sort | uniq -d
```

Take the `http>` lines for one duplicated digest and read the `<t>` column:
the gap between them is the wait the library took. The waits are drawn from a
window that doubles with each attempt — one second, then two, then four — so
they grow as a part runs out of attempts, though any single one may be short.
`grep -c '^http!' push.log` counts the failures that caused them, the summary
line's `failed=` agrees with that count, and the push still exits 0.

### Nothing named is nothing found

```
./bin/bigoci pull -plain-http 127.0.0.1:5050/team/model:absent /tmp/x.bin
echo "exit=$?"   # 3
```

The second failure line names the sentinel. That is `errors.Is` demonstrated
from a shell.

### A transfer nobody may make exits 6

Every run uses the credentials `docker login` stores, with no flag to switch
that off. The way to prove a run carried none is to give it a configuration
directory with nothing in it:

```
DOCKER_CONFIG=$(mktemp -d) ./bin/bigoci pull ghcr.io/you/private:v1 /tmp/x.bin
echo "exit=$?"   # 6
```

```
bigoci: unauthorized: pull ghcr.io/you/private:v1 to /tmp/x.bin: fetch the manifest: …
bigoci: matched sentinel bigoci.ErrUnauthorized (exit 6)
```

That is the whole authentication story from a shell: the environment says what
the run may see, and the exit code says what came of it. The same recipe with a
wrong token in the configuration exits 6 as well, and the `-debug` log shows one
token request and no waiting — a refused credential is not worth another
attempt.

### An authenticated push

This one needs a registry that authenticates, so it is written against GHCR
rather than the local zot every other recipe uses. Nothing about the command
line changes:

```
docker login ghcr.io
./bin/bigoci push -part-size 16MiB -debug /tmp/model.bin \
  ghcr.io/you/model:v1 2>auth.log
grep '^bigoci: http ' auth.log
```

Two things read differently than they do against zot. The summary line reports
`failed=1` or more — the 401 that carries the challenge is the protocol working
— and `other=` counts the token exchanges rather than zero.

### No credential in the log

The `-debug` log renders the *scheme* a request authenticated with and never
the credential itself. Prove it on the log the recipe above wrote:

```
grep -c "$TOKEN" auth.log                    # must be 0
grep -o 'auth=[a-z]*' auth.log | sort -u     # the positive control
```

The first grep proves the credential is absent. The second is the control that
proves the grep would have found something: `auth=bearer` on the lines that
carried one means the instrument was looking at authenticated requests, not at
an empty log. A log where every line reads `auth=none` proves nothing at all.

### No credential leaves for storage

GHCR answers a blob read with a `307` at signed object storage, so a pull makes
two requests per part: one to `ghcr.io`, and one to whatever host it named.
The second is the one that matters, and **a pull that works says nothing about
it** — that storage answers `200` to a request carrying the registry's bearer
token exactly as happily as to a clean one. Docker Hub's CloudFront does the
same. The log is the evidence, not the transfer:

```
./bin/bigoci pull -debug ghcr.io/you/model:v1 /tmp/back.bin 2>redirect.log
grep '^http> ' redirect.log | grep -v ' https://ghcr.io/' >off-registry.log

wc -l <off-registry.log                     # at least one line per part
grep -cv 'auth=none' off-registry.log       # 0
grep -c '^http> .*class=blob-read' redirect.log # two per part here: registry hop + storage read
grep -c '^http> .*class=other' redirect.log # at least 1
```

Three counts, and all three have to hold:

- **Presence.** There is at least one off-`ghcr.io` request line per part. A
  count of zero is a pull that never left the registry, and every line below it
  is then about nothing.
- **Universality.** No off-registry line reads anything but `auth=none`. The
  key is the host and the `auth=` field, never `class=`: what a storage URL's
  path looks like is the storage provider's choice, so the same request lands
  in `blob-read` at one registry and in `other` at the next. `class=` is
  corroboration here, not the key.
- **The instrument sees traffic.** At least one `class=other` request line
  exists, which is the token exchange. Together with the `challenge=` field on
  the `http<` line before it, that is the proof the tap is watching the
  authenticated path — the same reason the recipe above wants one `auth=bearer`
  line.

Nothing in that log can carry a signature: the query of every URL is rendered
with its values elided, and the library builds no error that names a signed
location either.

### The same gates, run by CI

Everything above is a recipe a person runs. `.github/workflows/conformance.yml`
runs the same gates against GHCR in this order: the refusal with an empty
`DOCKER_CONFIG`, a multi-part round trip, the counted no-leak check over that
pull's log, a digest-only round trip, and a credential the registry refuses.
The rows live in `cli/conformance_test.go` behind a `conformance` build tag, so
no ordinary build and no per-commit CI run compiles them.

Three things to know before reading a run:

- **Nothing triggers it but a person.** `workflow_dispatch` is its only
  trigger. It pushes real packages with the repository's own token and deletes
  them again, which is not something a pull request should do.
- **The evidence is the logs, not the green tick.** Every row writes the
  `-debug` log it captured — before it asserts anything, so a failed row leaves
  its evidence too — and the job uploads all of them as one artifact. Those
  files are what a journal entry quotes. Uploading them is safe for the reason
  this whole section is: the tap renders schemes and never credentials, elides
  every query value but a verified digest, and logs no body at all.
- **The refusal runs first, and it is the whole argument.** Every row after it
  differs only in which directory `DOCKER_CONFIG` names. If the target
  repository ever stops being private, that first row fails and the run stops,
  rather than four green rows claiming something they no longer show.

Each run writes to `run-<run id>-<attempt>` tags, which is what keeps a stale
artifact from answering a later run's pull, and what makes a version the
cleanup step could not delete identifiable by hand.

To run the same rows yourself against a repository you own:

```
docker login ghcr.io
cd cli
BIGOCI_CONFORMANCE_REPO=ghcr.io/you/bigoci/conformance \
BIGOCI_CONFORMANCE_DOCKER_CONFIG="$HOME/.docker" \
BIGOCI_CONFORMANCE_LOG_DIR=/tmp/conformance \
  go test -tags conformance -count=1 -v .
```

`BIGOCI_CONFORMANCE_REPO` unset skips the whole suite. The credential
directory is named separately rather than read from `DOCKER_CONFIG` because
this package's tests empty that variable on purpose, so that no other test can
reach a real credential.

One more thing about that credential directory: it must hold a `config.json`
with a plain base64 `auth` entry. This package's tests run with an empty
`PATH`, so a login that stored its secret in a credential helper —
`"credsStore": "osxkeychain"`, which is what Docker Desktop writes — cannot
be read here. If yours did, write a config by hand into a scratch directory:

```
d=$(mktemp -d)
printf '{"auths":{"ghcr.io":{"auth":"%s"}}}' "$(printf 'USER:TOKEN' | base64)" >"$d/config.json"
BIGOCI_CONFORMANCE_DOCKER_CONFIG="$d" ...
```

### Resuming an interrupted pull

Interrupt a pull part way through and read what the second one fetches. Start
from the four-part push already on the record:

```
./bin/bigoci pull -plain-http "127.0.0.1:5050/team/model@$d" /tmp/resume.bin
# Ctrl-C once, part way through
echo "exit=$?"                        # 130
ls -l /tmp/resume.bin.bigoci-partial  # present, at the full size
ls /tmp/resume.bin                    # "No such file or directory"
```

The destination does not exist — it never appears early — and the partial file
does, at the full 200 MiB: the file is sized up front, so its length says
nothing about how much arrived.
Over loopback this pull is over in a couple of seconds, so use a larger file or
put the toxiproxy from the retry recipe in front of the registry if the
interrupt keeps landing after the last part.

Now pull again and count the blob reads:

```
./bin/bigoci pull -plain-http -debug "127.0.0.1:5050/team/model@$d" \
  /tmp/resume.bin 2>resume.log
grep '^bigoci: http ' resume.log
```

How many parts the Ctrl-C left behind depends on where it landed, so the
numbers vary run to run. For a pull that had finished two of the four parts,
the summary reads:

```
bigoci: http requests=3 failed=0 blob-check=0 (0 hit, 0 miss) blob-write=0 upload-open=0 blob-read=2 manifest-read=1 manifest-write=0 manifest-check=0 other=0
```

`blob-read=` is one request per part, so with no retries in the log it is the
count of parts that were missing, and `manifest-read=1` is the one request a
resume always makes: the manifest is what names the digests every part is
checked against, so it is fetched before anything can be verified. A cold pull
of this file reads four blobs, so `blob-read=2` means two parts came off the
disk.

Which two is the digest grep, the same idiom the retry recipe uses:

```
grep 'http> .*class=blob-read' resume.log \
  | grep -o 'blobs/sha256:[0-9a-f]\{64\}' | sort
```

Those digests are a subset of the manifest's layers, in whatever order the
workers got to them.

The gate at the other end is a partial that is already complete. Pull once to
get a good file, then put it back under the partial name and pull again:

```
./bin/bigoci pull -plain-http "127.0.0.1:5050/team/model@$d" /tmp/whole.bin
mv /tmp/whole.bin /tmp/whole.bin.bigoci-partial
./bin/bigoci pull -plain-http -debug "127.0.0.1:5050/team/model@$d" /tmp/whole.bin 2>whole.log
grep '^bigoci: http ' whole.log
shasum -a 256 /tmp/model.bin /tmp/whole.bin
```

```
bigoci: http requests=1 failed=0 blob-check=0 (0 hit, 0 miss) blob-write=0 upload-open=0 blob-read=0 manifest-read=1 manifest-write=0 manifest-check=0 other=0
```

`blob-read=0 manifest-read=1` is the pull-side twin of the warm re-push's
`blob-write=0`: everything was already there, every part hashed to what the
manifest names, and the only request made was the one that said so. The two
`shasum` lines still match, which is what makes it a resume and not a shortcut.

That pull committed, so the partial is now the destination. Put it back, flip a
byte inside it, and exactly one part comes back:

```
mv /tmp/whole.bin /tmp/whole.bin.bigoci-partial
b=$(xxd -p -l1 -s1000000 /tmp/whole.bin.bigoci-partial)
printf "\x$(printf '%02x' $((0x$b ^ 0xff)))" \
  | dd of=/tmp/whole.bin.bigoci-partial bs=1 seek=1000000 conv=notrunc
./bin/bigoci pull -plain-http -debug "127.0.0.1:5050/team/model@$d" /tmp/whole.bin 2>flip.log
grep -c 'http> .*class=blob-read' flip.log   # 1
shasum -a 256 /tmp/model.bin /tmp/whole.bin
```

The `xxd` round trip inverts the byte that is there, whatever it is — writing
a fixed byte would silently change nothing the one time in 256 it matches.

One blob read, for the part the byte fell in, and the hashes match again: the
part was refetched over the damage.

## Honest caveats

**`dur` is time to headers, not throughput.** On a `GET` of a large blob it is
time to first byte; the body streams afterwards and is not timed. On a `PUT` it
is closer to the whole upload, because the server answers after reading the
body. The two are not comparable, and neither is a throughput measurement.

**Redirect hops are visible, and what they carry is the library's decision.**
A registry that redirects blob reads to storage shows both hops, because the
library derives the client that follows a redirect from the caller's rather
than building one of its own: the re-issue crosses this tap exactly as the
first request did, and so do the token exchanges, which show up as
`class=other` lines. What a hop carries is no longer the standard library's
rule either — automatic following is off, and each hop is built fresh with a
two-header allow-list. A log with a `307` and no follow-up request line
means one of two things: the instrument went blind, or the library refused
the location — an empty or userinfo-carrying `Location`, a plain-http
downgrade, a fourth hop. The failure line is what tells you which: a refusal
ends the run with an error naming the registry request, while a blind
instrument shows a transfer that carried on regardless.

**Against a redirecting registry, one part read is two requests.** The `307`
from the registry is one and the read of the location it named is the other.
Which class the second lands in depends on the path the storage provider
chose: GHCR's signed URLs keep a `/blobs/` segment, so `blob-read=` counts two
per part there, while a provider whose paths look different puts the same
request in `other`. Either way, every count in the resume recipe above —
`blob-read=` as the number of parts that were missing, one request per part —
is true of the local zot it is written against and of nothing else.

**The counters are HTTP-level inference, not library truth.** They are what an
observer outside the library can honestly say. A count that disagrees with the
library is a question about this instrument first.

**`class=` is a guess from a URL.** A registry that lays out its API
differently will land requests in `other`. That is the honest answer, not a
failure.

**The provenance line can name the wrong commit in a worktree.** The first
`-debug` line reports what the Go toolchain stamped into the binary, and building
inside a `git worktree` checkout has been observed to stamp the parent
checkout's commit instead: `vcs.revision` named HEAD's parent, and
`vcs.modified` said `false` with uncommitted changes in the tree. When
provenance matters, cross-check the binary directly:

```
go version -m ./bin/bigoci | grep vcs
git rev-parse HEAD
```

If those disagree, trust `git` and treat the log's `vcs=` field as a hint about
which build ran, not as proof of which source it was built from.

## Limits

- No progress output. It arrives with the library phase that implements it.
- No credentials flag. Authentication is the library's `docker login`
  credentials, always, and the way to run without them is an empty
  `DOCKER_CONFIG`.
- No environment variables, no configuration file, no shell completions. Flags
  are the whole interface.
- No `-` operands for standard input or standard output, and there cannot be.
- No `version` command.

The automated tests drive the whole program in process, including one push, warm
re-push, and pull against an in-process fake registry, so the exit codes, both
streams, the request log, and the summary line are all covered. Three things are
not, deliberately:

- **The usage blocks byte for byte.** Tests assert that specific wording is
  present — the flag names, the defaults read from the library, the sentences pull
  makes about `-part-size` and `-title` — but nothing pins a whole block, so
  reflowing help text is not a test failure.
- **`main`.** It builds the environment, installs the signal handler, and calls
  `run`. There is nothing in it a test could check that `run`'s own tests do not.
- **Signal delivery end to end.** Tests record a signal directly and check what
  the exit path does with it. Nothing sends a real SIGINT to a real process and
  watches a transfer unwind; the manual gates above do that.

Running against a real registry is still the manual gate, and always will be: a
fake answers the way it was written to. The conformance job is that gate
written down rather than replaced — it runs the same rows against GHCR, and it
still runs only when somebody triggers it.

## License

Dual-licensed under Apache-2.0 and MIT, at your option, inherited from the
library. Having its own `go.mod` changes nothing about that.
