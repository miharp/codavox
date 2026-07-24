#!/bin/sh
set -e

# Register the shipped units so they can be started, but do not enable or start
# anything: which node runs the agent, the publisher, or the deploy server is
# the operator's (or the Forge module's) choice, so installing the package is
# never itself a configuration change.
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi
