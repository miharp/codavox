#!/bin/bash
# Build the codavox package repository as a directory of static files.
#
#   build.sh <packages-dir> <site-dir> <gpg-key-id>
#
# The repository is derived, not maintained: every run rebuilds it in full
# from whatever rpm and deb files are in <packages-dir>, which the workflow
# fills from GitHub Releases. There is no state to migrate and nothing to
# get out of step with the releases page, which stays the source of truth.
#
# What is signed is the repository metadata — repomd.xml for dnf, Release for
# apt — and not the packages themselves. The metadata carries every package's
# SHA-256, so a signed index vouches for every file below it; this is how apt
# has always worked, and dnf verifies it with repo_gpgcheck. It also means the
# packages here are byte-for-byte the release assets, so checksums.txt on the
# releases page still describes them.
#
# Needs createrepo_c, apt-ftparchive (apt-utils), and gpg with the key's
# secret half. GPG_PASSPHRASE, if set, unlocks it non-interactively.
set -euo pipefail

pkgs="${1:?packages directory}"
site="${2:?site directory}"
key="${3:?gpg key id}"
base_url="${BASE_URL:-https://mikeharp.com/codavox}"

sign() { # sign <args...> — gpg with the key, unattended when a passphrase is given
  if [ -n "${GPG_PASSPHRASE:-}" ]; then
    gpg --batch --yes --pinentry-mode loopback --passphrase-fd 3 -u "$key" "$@" 3<<<"$GPG_PASSPHRASE"
  else
    gpg --batch --yes -u "$key" "$@"
  fi
}

rm -rf "$site"
install -d "$site/rpm" "$site/deb/pool/main/c/codavox"

# Pages runs Jekyll over the tree unless told not to, and Jekyll drops files it
# considers internal; nothing here is for Jekyll.
: > "$site/.nojekyll"

# --- the public key, in both encodings a package manager may ask for -------
gpg --export --armor "$key" > "$site/codavox.asc"
gpg --export "$key" > "$site/codavox.gpg"

# --- rpm ------------------------------------------------------------------
# One repository for both architectures: dnf filters by the running system's
# arch, and a single baseurl is one less thing to get wrong in a .repo file.
cp "$pkgs"/*.rpm "$site/rpm/"
createrepo_c --quiet "$site/rpm"
sign --detach-sign --armor --output "$site/rpm/repodata/repomd.xml.asc" "$site/rpm/repodata/repomd.xml"

cat > "$site/rpm/codavox.repo" <<REPO
[codavox]
name=codavox - versioned Puppet code distribution for OpenVox
baseurl=${base_url}/rpm
enabled=1
# The packages are unsigned; the metadata that names their checksums is signed.
gpgcheck=0
repo_gpgcheck=1
gpgkey=${base_url}/codavox.asc
REPO

# --- deb ------------------------------------------------------------------
cp "$pkgs"/*.deb "$site/deb/pool/main/c/codavox/"
# Filename: fields are relative to where apt-ftparchive runs, so run it at the
# repository root; one index per architecture, from the shared pool.
( cd "$site/deb"
  for arch in amd64 arm64; do
    install -d "dists/stable/main/binary-$arch"
    apt-ftparchive --arch "$arch" packages pool > "dists/stable/main/binary-$arch/Packages"
    gzip -9 -k -f "dists/stable/main/binary-$arch/Packages"
  done
  apt-ftparchive \
    -o APT::FTPArchive::Release::Origin=codavox \
    -o APT::FTPArchive::Release::Label=codavox \
    -o APT::FTPArchive::Release::Suite=stable \
    -o APT::FTPArchive::Release::Codename=stable \
    -o "APT::FTPArchive::Release::Architectures=amd64 arm64" \
    -o APT::FTPArchive::Release::Components=main \
    release dists/stable > dists/stable/Release
)
sign --clearsign --output "$site/deb/dists/stable/InRelease" "$site/deb/dists/stable/Release"
sign --detach-sign --armor --output "$site/deb/dists/stable/Release.gpg" "$site/deb/dists/stable/Release"

# --- the front page -------------------------------------------------------
latest=$(for f in "$site"/rpm/codavox_*_linux_amd64.rpm; do basename "$f"; done \
  | sed 's/^codavox_\(.*\)_linux_amd64\.rpm$/\1/' | sort -V | tail -1)
cat > "$site/index.html" <<HTML
<!doctype html>
<meta charset="utf-8">
<title>codavox package repository</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:46em;margin:3em auto;padding:0 1em}pre{background:#f4f4f4;padding:.8em;overflow-x:auto}</style>
<h1>codavox package repository</h1>
<p>Packages for <a href="https://github.com/miharp/codavox">codavox</a>, rebuilt from
<a href="https://github.com/miharp/codavox/releases">GitHub Releases</a> on every release.
Latest: <strong>${latest}</strong>. The repository metadata is signed with
<a href="codavox.asc">this key</a>; the packages are the release assets, unchanged.</p>
<h2>RPM (Rocky, RHEL, AlmaLinux, CentOS Stream)</h2>
<pre>curl -fsSL ${base_url}/rpm/codavox.repo -o /etc/yum.repos.d/codavox.repo
dnf install codavox</pre>
<h2>DEB (Debian, Ubuntu)</h2>
<pre>curl -fsSL ${base_url}/codavox.asc -o /etc/apt/keyrings/codavox.asc
echo "deb [signed-by=/etc/apt/keyrings/codavox.asc] ${base_url}/deb stable main" &gt; /etc/apt/sources.list.d/codavox.list
apt-get update &amp;&amp; apt-get install codavox</pre>
<p>See <a href="https://github.com/miharp/codavox/blob/main/docs/installation.md">installation.md</a>.</p>
HTML

rpms=("$site"/rpm/*.rpm); debs=("$site"/deb/pool/main/c/codavox/*.deb)
echo "built $site: ${#rpms[@]} rpm, ${#debs[@]} deb, latest $latest"
