#!/bin/bash
# Provision the primary node: boot the Puppet CA, seed an environment, and start
# the codavox publisher and deploy server. Runs inside the primary container.
#
# This is plain shell on purpose: it is the worked example that codavox needs no
# ovadm (or any other provisioner) to stand up.
set -euo pipefail
export PATH="/opt/puppetlabs/bin:$PATH"

STAGING=/etc/puppetlabs/code-staging
API_TOKEN="${API_TOKEN:-integration-token}"
WEBHOOK_SECRET="${WEBHOOK_SECRET:-integration-secret}"

echo "[primary] configuring puppetserver as the CA"
puppet config set --section main certname primary
puppet config set --section main dns_alt_names primary,puppet
# Autosign in this throwaway CA so the compiler enrolls without a manual sign.
echo '*' > /etc/puppetlabs/puppet/autosign.conf

echo "[primary] starting puppetserver (generates the CA on first boot)"
systemctl enable --now puppetserver
for _ in $(seq 1 60); do
  if curl -sk --max-time 3 https://localhost:8140/status/v1/simple 2>/dev/null | grep -q running; then
    break
  fi
  sleep 3
done

echo "[primary] seeding ${STAGING}/production"
mkdir -p "${STAGING}/production/manifests"
cat > "${STAGING}/production/manifests/site.pp" <<'PP'
node default {
  notify { 'codavox: served through a static catalog': }
  file { '/tmp/codavox-managed':
    ensure  => file,
    content => "codavox integration fixture\n",
  }
}
PP
cat > "${STAGING}/production/environment.conf" <<'CONF'
# Minimal environment seeded by the codavox integration harness.
CONF

echo "[primary] writing codavox config and secrets"
mkdir -p /etc/codavox
printf '%s' "$API_TOKEN"      > /etc/codavox/api.token
printf '%s' "$WEBHOOK_SECRET" > /etc/codavox/webhook.secret
# api_token and secret are file paths, matching the --api-token/--secret flags.
cat > /etc/codavox/config.yaml <<CFG
staging: ${STAGING}
ssldir: /etc/puppetlabs/puppet/ssl
certname: primary

deploy_server:
  api_token: /etc/codavox/api.token
  secret: /etc/codavox/webhook.secret
CFG

echo "[primary] starting the codavox publisher and deploy server"
systemctl enable --now codavox-publish
systemctl enable --now codavox-deploy-server

echo "[primary] provisioned"
