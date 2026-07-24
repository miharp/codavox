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

echo "[compiler] writing the codavox agent config"
mkdir -p /etc/codavox
# A short interval keeps the harness quick; keep is small so the prune assertion
# has something to reap.
cat > /etc/codavox/config.yaml <<CFG
ssldir: ${SSLDIR}
certname: ${CERTNAME}

agent:
  publisher: https://${PRIMARY_FQDN}:8150
  interval: 5s
  keep: 2
  min_age: 1s
CFG

echo "[compiler] starting the codavox agent and waiting for convergence"
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

echo "[compiler] starting puppetserver"
systemctl enable --now puppetserver
for _ in $(seq 1 60); do
  if curl -sk --max-time 3 https://localhost:8140/status/v1/simple 2>/dev/null | grep -q running; then
    break
  fi
  sleep 3
done

echo "[compiler] provisioned"
