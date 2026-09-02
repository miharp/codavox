# Your first server

One OpenVox Server that compiles its own catalogs, serving its own code with a
`code_id` in every catalog, in eight steps. This is the case the README
promises, and it is the base of every larger estate: compilers are added to it
afterwards, not set up differently. Run everything as root on that
server. Every command here is one the
[integration harness](../test/integration/README.md) runs against a real OpenVox
Server, and the sequence was run by hand, as written, before it was written
down.

**1. Install the package** as in [installation.md](installation.md#install).

**2. Make sure r10k can deploy your control repo.** codavox has no control-repo
setting: `codavox deploy` runs r10k as it is already set up on this node. If
r10k deploys here today, skip to step 3. On a fresh server, install it into
OpenVox's bundled Ruby and point it at your control repo:

```console
/opt/puppetlabs/puppet/bin/gem install r10k
```

```yaml
# /etc/puppetlabs/r10k/r10k.yaml
sources:
  puppet:
    remote: https://github.com/example/control-repo.git
    basedir: /etc/puppetlabs/code/environments
```

That `basedir` is the directory codavox observes. See
[deploying.md](deploying.md#where-your-control-repo-is-configured).

**3. Configure codavox.** The package ships this file fully commented out; these
are the only settings a single node needs:

```yaml
# /etc/codavox/config.yaml
basedir: /etc/puppetlabs/code/environments

agent:
  # This node's certname, never localhost: the publisher presents this node's
  # Puppet certificate, and the name has to verify against it.
  publisher: https://puppet.example.com:8150
  # A fresh server re-reads its environment on every compile, so there is no
  # cache to expire. Step 7 says when to turn this back on.
  flush_environment_cache: false
```

Nothing about authorization: the publisher admits its own node without being
told to, and an allowlist only matters once a compiler joins.

**4. Start the publisher and the agent.**

```console
systemctl enable --now codavox-publish codavox-agent
```

**5. Deploy.** The command is the one you know from `puppet-code`:

```console
$ codavox deploy production --wait
production    deployed    c58958951addab91a7349432328b3fdcc6c273378795066cd5fcf06de62666d1    (commit 1d587d2)    serving
```

r10k ran, the resolved tree was sealed to that `code_id`, and the publisher is
serving it.

**6. Watch this node pull it.** The agent polls every 30 seconds by default, so
within one poll the node reports the `code_id` the deploy printed:

```console
$ codavox compilers
COMPILER            ENVIRONMENT  CODE_ID       COMMIT        LAST POLL
puppet.example.com  production   c58958951add  1d587d2b57e8  6s ago

$ codavox code-id production
c58958951addab91a7349432328b3fdcc6c273378795066cd5fcf06de62666d1
```

The second command is the one OpenVox Server will run on every compile, and it
prints the whole digest because the server needs all of it. **Do not go on until
it matches the deploy.** Step 7 points the server at a directory the agent
fills, and on a node that compiles its own catalogs, doing that early stops
catalog compilation with no Puppet run left to repair it.

**7. Wire OpenVox Server to codavox.**

```console
cat > /etc/puppetlabs/puppetserver/conf.d/versioned-code.conf <<'HOCON'
versioned-code: {
  code-id-command: "/usr/bin/codavox-code-id"
  code-content-command: "/usr/bin/codavox-code-content"
}
HOCON
puppet config set --section main environmentpath /opt/puppetlabs/codavox/environments
systemctl restart puppetserver
```

If this server sets `environment_timeout`, as a production server does for
compile speed, the agent has to expire the cached environment after every swap
or the server keeps compiling the old tree under the new `code_id`. Add the
`auth.conf` rule from [Wiring into
puppetserver](commands.md#wiring-into-puppetserver), admitting this node by
certname, and set `flush_environment_cache` back to `true`.

**8. Prove it.** Compile a catalog and read the version it was pinned to:

```console
$ puppet agent -t
Notice: hello from codavox
Notice: Applied catalog in 0.01 seconds

$ grep -o '"code_id":"[^"]*"' /opt/puppetlabs/puppet/cache/client_data/catalog/puppet.example.com.json
"code_id":"c58958951addab91a7349432328b3fdcc6c273378795066cd5fcf06de62666d1"
```

That is a static catalog doing its job: every file this run fetches comes from
exactly that version, whatever is deployed meanwhile. Commit to the control
repo, deploy again, and the two move together.

## Next

**Adding a compiler** is the same package and the same `agent` settings pointed
at this primary, plus the two things a second node needs: a certificate that
carries `pp_role: openvox_compiler` (or its certname in the publisher's
`allow_certnames`), and the `auth.conf` rule above so its agent can expire its
own server's cache. Wire it in the same order. See
[installation.md](installation.md), which covers moving one compiler
before the rest.

**Running this in production, or on more than one node?** Use the
[miharp/puppet-codavox](https://github.com/miharp/puppet-codavox) module rather
than doing the steps above by hand. Its `codavox::primary` class removes the
ordering hazard in step 6 for you: it waits for the `codavox_environments` fact
to report the environment converged before it wires `environmentpath`. Then
read [production.md](production.md) for ports, sizing, failure modes, and
what to monitor.

**Push-to-deploy and CI:** run `codavox deploy-server` on the primary, a push
webhook and a token-authenticated deploy API with status and history, the way
Code Manager's webhook and API work. See
[deploy-server.md](deploy-server.md).
