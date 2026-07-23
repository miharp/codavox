# Sealing

Sealing derives a `code_id` from a staged environment tree, and optionally
writes the artifact a compiler will receive.

The `code_id` must be a **pure function of tree content**. The same code sealed
on two machines, at different times, by different users, must produce the same
id — otherwise "these compilers report the same code_id" stops meaning "these
compilers serve the same code", and the whole system rests on that.

## Usage

```console
codavox seal /etc/puppetlabs/code-staging/production
```

```text
3224ddbe7e3d05fe236823b4596fac8eeebc9ceb38c47d551de912b496884beb
```

Write the artifact at the same time:

```console
codavox seal /path/to/environment --archive production.tar.gz
```

Inspect the canonical manifest, which is what actually gets hashed:

```console
codavox seal /path/to/environment --manifest
```

```text
codavox-tree-v1
file 0644 950532c7...5805d8 13 Puppetfile
file 0644 2673a883...32d1   17 manifests/site.pp
file 0644 7232560a...5688ee 17 modules/apache/init.pp
```

**Sealing only reads the directory.** Staging remains r10k's job. Not owning
the deploy keeps the trust boundary small and lets existing r10k workflows
continue untouched.

## What is hashed

A canonical manifest is built and then hashed, rather than hashing files
directly. The manifest is reproducible by construction, and it can be printed —
when two trees disagree, diffing manifests shows exactly which entry differs.
A bare digest cannot tell you that.

Each entry records:

| field | notes |
|---|---|
| kind | `file` or `link` |
| mode | normalized to `0644` or `0755` — only the executable bit is meaningful |
| digest | SHA-256 of content, or of a symlink's target |
| size | content length, or target length |
| path | slash-separated, relative to the tree root |

Entries are sorted by path, so filesystem iteration order cannot affect the
result. The manifest is prefixed with a version string, so a future change to
the canonical form yields different ids rather than silently colliding with
ids produced by the old algorithm.

`code_id` is the hex SHA-256 of the manifest. Hex rather than base64 because
OpenVox Server's `CodeId` schema rejects `+` and `=`.

## What is deliberately excluded

**Modification times.** A file's mtime is not its content. Including it would
give every r10k redeploy of unchanged code a fresh `code_id`, so every compiler
would fetch and swap for no reason.

**Ownership and non-executable permission bits.** These vary with the umask of
whoever ran r10k. The executable bit *is* content — a script that loses `+x`
behaves differently — so it is kept.

**Empty directories.** Invisible to the manifest, matching git, so the id does
not depend on whether a transport preserves them.

**`.git`** — packfiles and reflogs differ between clones of identical content.

**`.r10k-deploy.json`** — embeds deploy timestamps. This one matters most:
including it would defeat the entire point, since it changes on every deploy.

Symlinks are hashed by their **target**, not by following them. Following would
double-count files inside the tree and could escape it entirely, making the id
depend on files outside the environment.

Sockets, devices, and FIFOs are rejected rather than skipped. They have no
reproducible content and no business in a Puppet environment, and failing is
better than sealing a tree that cannot be faithfully reproduced.

## Artifacts

`--archive` writes a gzipped tar that is **byte-reproducible** for identical
content. An archive that varied between runs could not be content-addressed,
cached, or deduplicated by a transport, and "did this artifact change?" would
be unanswerable without unpacking it.

Reproducibility comes from normalizing everything the filesystem supplies that
is not tree content: sorted entry order, epoch modification times, zeroed
uid/gid, dropped owner names, normalized modes, and a gzip header carrying no
timestamp or original filename.

Extraction confines every entry to the destination directory. An artifact
arrives over the network, so its contents are untrusted:

- entries with `..` components or absolute paths are refused
- symlinks with absolute targets, or relative targets that climb out of the
  tree, are refused
- file modes are normalized to `0644` or `0755`, discarding setuid, setgid and
  sticky bits — sealing never emits anything else, so a mode from an archive is
  either corruption or an attempt to plant a privileged file on every compiler

That second check matters more than it first appears. `os.Root` blocks
*following* an escaping symlink, but permits *creating* one — the link itself
is inside the root. codavox reads through `os.Root` and would be safe, but
OpenVox Server also serves files from the environment directory directly and
would follow such a link. Rejecting at extraction stops the planted link ever
reaching disk.

## Verifying reproducibility

Seal the same content on two different machines and compare. This was verified
across macOS/arm64 and Rocky 9 in a container, with different users, different
modification times, different umasks, and deliberately different
`.r10k-deploy.json` contents:

```text
host      3224ddbe7e3d05fe236823b4596fac8eeebc9ceb38c47d551de912b496884beb
container 3224ddbe7e3d05fe236823b4596fac8eeebc9ceb38c47d551de912b496884beb
```

The artifacts were byte-identical too.

If two machines disagree, diff the manifests rather than the digests:

```console
codavox seal /path/to/tree --manifest > a.manifest
```

The differing line names the file and shows whether content, mode, size, or
path is responsible.
