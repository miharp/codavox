# Static catalogs, and the four things people confuse

Static catalogs are worth understanding on their own terms before you reach for
codavox, because most of the confusion around them is vocabulary: several
similarly named settings sound like "the code version" and do unrelated jobs.
This page separates them, then says where codavox fits.

If you only remember one thing: **the compile log is the source of truth.**
`Compiled static catalog for ...` means it is working; `Compiled catalog
for ...` means it is not. Everything below explains why.

## The one question static catalogs answer

An agent applies a catalog over seconds or minutes, fetching module file content
from the server as it goes. If you deploy new code mid-run, the agent can fetch
the old version of one file and the new version of the next, and apply a mix.
That inconsistency is silent and hard to reproduce.

A static catalog closes that window. At compile time the server stamps the
catalog with a version, the **`code_id`**, and for the rest of the run the agent
asks for file content *at that `code_id`* rather than "latest". It gets the set
that matches the catalog it was handed, even if you deploy in the middle.

For that to work the server needs to answer two questions on demand, which it
delegates to two external commands (`versioned-code.conf` on OpenVox Server):

| command | question | returns |
|---|---|---|
| `code-id-command` | which version is current? | a `code_id` |
| `code-content-command` | give me file X at version Y | the file's bytes at that `code_id` |

Both or neither must be set; setting one makes the server fail to start. There
are no default scripts. That is the entire contract. See Puppet's
[static catalogs documentation](https://help.puppet.com/core/current/Content/PuppetCore/static-catalogs.htm)
and [`puppetserver.conf`](https://help.puppet.com/core/current/Content/PuppetCore/server/config_file_puppetserver.htm),
and [versioned-code-contract.md](versioned-code-contract.md) for the behavior
verified from source.

## The four names that get confused

| name | what it is | does it turn static catalogs on? |
|---|---|---|
| **`code_id`** | the version identity the two commands trade in | yes, this is the mechanism |
| **`config_version`** | a label printed in reports and run logs | no, unrelated feature |
| **`static_catalogs`** | a setting that already defaults to `true` | no, it is a prerequisite, not the switch |
| the **scripts** | how you answer the two commands | they are the implementation, not the feature |

Two of these cause almost all the trouble.

**`config_version` is not `code_id`.** `config_version` produces the human
version string you see in `Applying configuration version '...'`. It has nothing
to do with `code-id-command` and does not pin any file content. The standard
control repo already sets it, wired to a
[script](https://github.com/puppetlabs/control-repo/blob/production/scripts/config_version.sh)
whose last resort is `date +%s`. That is fine for a label and fatal as a
`code_id`, which must be stable and identical on every compiler. The two happen
to both be a git SHA in a typical setup, which is exactly why they get conflated.
Having `config_version` set tells you nothing about whether static catalogs work.

**`static_catalogs = true` is not enough on its own.** It defaults to `true`
already, and does nothing until `code-id-command` and `code-content-command` are
wired up. With them unset, the server asks for a `code_id`, gets nil back, and
quietly compiles an ordinary catalog. No error, no per-compile warning.

## How to check, in one line

Read the compile log. The server picks the log line from whether a `code_id`
reached the compiler:

```text
Compiled catalog for web01.example.com          # no code_id: not static
Compiled static catalog for web01.example.com   # code_id present: working
```

Nothing else, `config_version` included, changes which line you get.

## Why git alone cannot finish the job

Puppet's documentation shows the natural do-it-yourself implementation: git.
`git rev-parse HEAD` answers `code-id-command`, and `git show <code_id>:<path>`
answers `code-content-command`, both run in the environment directory.

This works for every file tracked in your control repo. It cannot work for a
file that lives in a module r10k installed from your Puppetfile: a Forge module,
or anything pulled from another repo. Those files are in no commit of the control
repo, so `git show` has nothing to return. A correct git script must then *fail*
for that path.

The tempting fix is to have the script fall back to reading the file from disk.
Do not. The on-disk file is the *current* version, not the version at the
requested `code_id`, so the fallback answers the one question the endpoint exists
to prevent: it serves post-deploy bytes under a catalog compiled before the
deploy. That failure is silent and exit-zero, which is worse than the loud error
it replaces. (A missing `code_id` should also fail, not fall back to a
timestamp, for the same reason.)

So a git-based implementation is correct only where it fails loudly on anything
it cannot serve at a version, and that means it cannot pin module file content at
all. On a control repo whose only `puppet:///` file sources are its own tracked
modules, that is fine. The moment a catalog sources content from a Puppetfile
module, git is out of answers.

## Where codavox fits

codavox distributes the *resolved* tree r10k produced, the modules included,
addressed by a content-hash `code_id`, and answers both commands from it
identically on every compiler. It never falls back to a wrong version: a request
for a `code_id` a compiler does not have is a hard error, not stale content.

That is the whole difference from the git approach. git can pin what your control
repo tracks; it cannot serve module content at a version. Distributing the
resolved tree is what makes `code-content` answer for every file in the catalog,
which is what static catalogs need to actually hold.

See [design.md](design.md) for why resolved trees, not Puppetfiles, are the unit
of distribution.
