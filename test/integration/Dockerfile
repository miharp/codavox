# Self-contained OpenVox Server host for codavox's integration harness — no ovadm.
#
# Bakes the OpenVox repo, the server package, and a supported Java so a run only
# has to *provision* (boot the CA, enroll the compiler, wire codavox), which is
# the part that proves you do not need ovadm. codavox itself is installed at run
# time from a package built from the current checkout, so the harness tests HEAD.
#
# rockylinux:9-ubi-init runs systemd as PID 1, so puppetserver and the codavox
# units run the way they do on a real host.
FROM rockylinux/rockylinux:9-ubi-init

ARG OVOX_MAJOR=8

# java-21 is a supported OpenVox runtime; install it explicitly so openvox-server
# does not pull an unsupported Java (current Rocky 9 resolves java 25, which
# OpenVox rejects). hostname is used by the provisioning scripts; curl is already
# present as curl-minimal, and asking for curl conflicts with it.
RUN rpm -Uvh --replacepkgs "https://yum.voxpupuli.org/openvox${OVOX_MAJOR}-release-el-9.noarch.rpm" \
 && dnf install -y -q openvox-server hostname java-21-openjdk-headless \
 && dnf clean all
