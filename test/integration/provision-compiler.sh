#!/bin/bash
# Provision the compiler node: enroll a compiler certificate against the primary
# CA, converge the codavox agent, then wire OpenVox Server to codavox — in that
# order, because a compiler wired before its agent has converged has no code to
# serve and its catalogs fail to compile. Runs inside the compiler container.
set -euo pipefail
export PATH="/opt/puppetlabs/bin:$PATH"

PRIMARY_FQDN="${1:-primary}"
CERTNAME=compiler
SSLDIR=/etc/puppetlabs/puppet/ssl

echo "[compiler] pointing puppet at the primary CA"
puppet config set --section main certname "$CERTNAME"
puppet config set --section agent server "$PRIMARY_FQDN"
puppet config set --section agent ca_server "$PRIMARY_FQDN"

echo "[compiler] requesting a certificate carrying pp_role=openvox_compiler"
# The publisher enforces pp_role on the client cert, so the compiler's Puppet
# cert must carry it. codavox reuses this same material for mutual TLS.
cat > /etc/puppetlabs/puppet/csr_attributes.yaml <<'YAML'
extension_requests:
  pp_role: openvox_compiler
YAML
puppet ssl bootstrap --waitforcert 5

echo "[compiler] configuring puppetserver to use the primary CA (compiler mode)"
cat > /etc/puppetlabs/puppetserver/conf.d/webserver.conf <<HOCON
webserver: {
    access-log-config: /etc/puppetlabs/puppetserver/request-logging.xml
    client-auth: want
    ssl-host: 0.0.0.0
    ssl-port: 8140
    ssl-cert: ${SSLDIR}/certs/${CERTNAME}.pem
    ssl-key: ${SSLDIR}/private_keys/${CERTNAME}.pem
    ssl-ca-cert: ${SSLDIR}/certs/ca.pem
    ssl-crl-path: ${SSLDIR}/crl.pem
}
HOCON
# Disable the local CA service so this node defers to the primary's CA.
ca_cfg=/etc/puppetlabs/puppetserver/services.d/ca.cfg
sed -i \
  -e 's|^puppetlabs.services.ca.certificate-authority-service/|#puppetlabs.services.ca.certificate-authority-service/|' \
  -e 's|^#puppetlabs.services.ca.certificate-authority-disabled-service/|puppetlabs.services.ca.certificate-authority-disabled-service/|' \
  "$ca_cfg"
rm -rf /etc/puppetlabs/puppetserver/ca

echo "[compiler] allowing the agent to expire this server's environment cache"
# After every swap the agent asks its own OpenVox Server to expire the cached
# environment, over DELETE /puppet-admin-api/v1/environment-cache. The shipped
# auth.conf has no rule for that path, so it falls to "puppetlabs deny all" —
# without this rule the server answers 403 and keeps compiling the tree it
# already parsed. The agent presents the compiler's own certificate, so the rule
# admits the pp_role that certificate carries — by OID, not by the short name
# `pp_role`: a compiler runs with its CA service disabled, and the admin API is
# then authorized with no OID-to-short-name map at all, so `pp_role` would
# match nothing and the request would be "denied by rule" with no further
# explanation. By hand this is one more entry in the rules array, and in Puppet
# it is a puppet_authorization::rule. Edited with the hocon gem openvox-agent
# ships, the same way that module does it.
/opt/puppetlabs/puppet/bin/ruby - <<'RUBY'
require 'hocon'
require 'hocon/config_value_factory'
require 'hocon/parser/config_document_factory'

path = '/etc/puppetlabs/puppetserver/conf.d/auth.conf'
rule = {
  'match-request' => {
    'path'   => '/puppet-admin-api/v1/environment-cache',
    'type'   => 'path',
    'method' => 'delete',
  },
  'allow'      => { 'extensions' => { '1.3.6.1.4.1.34380.1.1.13' => 'openvox_compiler' } },
  'sort-order' => 200,
  'name'       => 'codavox environment cache flush',
}
rules = Hocon.load(path)['authorization']['rules']
rules << rule unless rules.any? { |r| r['name'] == rule['name'] }
doc = Hocon::Parser::ConfigDocumentFactory.parse_file(path)
doc = doc.set_config_value('authorization.rules', Hocon::ConfigValueFactory.from_any_ref(rules))
File.write(path, doc.render)
RUBY

# One JRuby instance, so a catalog compile always lands on the same environment
# cache. The cache-flush assertion first proves the cache goes stale without the
# flush, and a second instance that had never cached the environment would parse
# it fresh and pass that check for the wrong reason.
/opt/puppetlabs/puppet/bin/ruby - <<'RUBY'
require 'hocon'
require 'hocon/config_value_factory'
require 'hocon/parser/config_document_factory'

path = '/etc/puppetlabs/puppetserver/conf.d/puppetserver.conf'
doc = Hocon::Parser::ConfigDocumentFactory.parse_file(path)
doc = doc.set_config_value('jruby-puppet.max-active-instances', Hocon::ConfigValueFactory.from_any_ref(1))
File.write(path, doc.render)
RUBY

echo "[compiler] writing the codavox agent config"
mkdir -p /etc/codavox
# A short interval keeps the harness quick; keep is small so the prune assertion
# has something to reap. flush_environment_cache is written out at its default
# so features.sh can flip it to reproduce a stale cache, then flip it back.
cat > /etc/codavox/config.yaml <<CFG
ssldir: ${SSLDIR}
certname: ${CERTNAME}

agent:
  publisher: https://${PRIMARY_FQDN}:8150
  interval: 5s
  keep: 2
  min_age: 1s
  flush_environment_cache: true
CFG

echo "[compiler] starting the codavox agent and waiting for convergence"
# puppetserver is not running yet, so the agent's first flush fails and it
# logs an error; it stays owed and lands on the first poll after the server
# is up. The order stays agent-first on purpose: a compiler wired before its
# agent has converged has no code to serve.
systemctl enable --now codavox-agent
for _ in $(seq 1 40); do
  codavox-code-id production >/dev/null 2>&1 && break
  sleep 3
done
codavox-code-id production

echo "[compiler] wiring OpenVox Server to codavox"
cat > /etc/puppetlabs/puppetserver/conf.d/versioned-code.conf <<'HOCON'
versioned-code: {
    code-id-command: /usr/bin/codavox-code-id
    code-content-command: /usr/bin/codavox-code-content
}
HOCON
puppet config set --section main environmentpath /opt/puppetlabs/codavox/environments
puppet config set --section server static_catalogs true
# Cache environments forever, as a production compiler does for compile speed.
# With this set, only the agent's flush makes a deploy visible to the server;
# at the default of 0 the server re-reads the environment on every compile and
# a missing flush would never show.
puppet config set --section server environment_timeout unlimited

echo "[compiler] starting puppetserver"
systemctl enable --now puppetserver
for _ in $(seq 1 60); do
  if curl -sk --max-time 3 https://localhost:8140/status/v1/simple 2>/dev/null | grep -q running; then
    break
  fi
  sleep 3
done

echo "[compiler] provisioned"
