#!/bin/sh
set -eu

# Signing is the default; development packages require an explicit opt-out.
signing_key=${S2PLUGIN_KEY:-"$HOME/.config/sub-data-save/publisher-v1.private.pem"}
if [ "${1:-}" = --install-signing-key ]; then
  node - "${2:-$signing_key}" <<'NODE'
const fs = require('node:fs')
const path = require('node:path')
const os = require('node:os')
const crypto = require('node:crypto')
const dir = path.join(os.homedir(), '.config', 'sub-data-save')
const dest = path.join(dir, 'publisher-v1.private.pem')
// This public key is pinned to this plugin ID by the gateway.
const trusted = Buffer.from('7Vmw+FaG7muaH0nqY+Hx5YwSR8KP5ieREtOJDiQZ2T4=', 'base64url')
function check(bytes) {
  const pub = crypto.createPublicKey(crypto.createPrivateKey(bytes)).export({format:'jwk'})
  if (pub.crv !== 'Ed25519' || !Buffer.from(pub.x, 'base64url').equals(trusted)) {
    throw Error('Selected private key does not match the trusted publisher.')
  }
}
try {
  const bytes = fs.readFileSync(process.argv[2])
  check(bytes)
  fs.mkdirSync(dir, {recursive:true, mode:0o700})
  fs.chmodSync(dir, 0o700)
  const tmp = fs.mkdtempSync(path.join(dir, '.install-'))
  const staged = path.join(tmp, 'key')
  try {
    fs.writeFileSync(staged, bytes, {flag:'wx', mode:0o600})
    try { fs.linkSync(staged, dest) } catch (e) {
      if (e.code !== 'EEXIST') throw e
      if (!fs.lstatSync(dest).isFile()) throw Error('Existing signer must be a regular file.')
      check(fs.readFileSync(dest))
      fs.chmodSync(dest, 0o600)
    }
  } finally { fs.rmSync(tmp, {recursive:true, force:true}) }
  console.log('Trusted publisher key configured for this user.')
} catch (e) {
  console.error('Publisher key setup failed:', e.code || e.message)
  process.exitCode = 1
}
NODE
  exit $?
fi
if [ "${S2PLUGIN_UNSIGNED:-0}" != 1 ] && [ ! -f "$signing_key" ]; then
  printf '%s\n' 'Missing publisher key: set S2PLUGIN_KEY, or S2PLUGIN_UNSIGNED=1 for local development.' >&2
  exit 1
fi
export S2PLUGIN_KEY="$signing_key"
target_os=${GOOS:-linux}
target_arch=${GOARCH:-amd64}
target="$target_os-$target_arch"

mkdir -p "dist/$target/bin" "dist/$target/ui"
GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -o "dist/$target/bin/sub-data-save" ./cmd/sub-data-save
cp ui/index.html "dist/$target/ui/index.html"

node - "manifest.source.json" "dist/$target" "$target" <<'NODE'
const fs = require('node:fs')
const crypto = require('node:crypto')
const [source, root, target] = process.argv.slice(2)
const manifest = JSON.parse(fs.readFileSync(source, 'utf8'))
manifest.runtimes = { [target]: { path: 'bin/sub-data-save' } }
manifest.files = {}
for (const path of ['bin/sub-data-save', 'ui/index.html']) {
  manifest.files[path] = crypto.createHash('sha256').update(fs.readFileSync(`${root}/${path}`)).digest('hex')
}
const raw = Buffer.from(`${JSON.stringify(manifest, null, 2)}\n`)
fs.writeFileSync(`${root}/manifest.json`, raw)
if (process.env.S2PLUGIN_UNSIGNED !== '1') {
  const key = crypto.createPrivateKey(fs.readFileSync(process.env.S2PLUGIN_KEY))
  const signature = crypto.sign(null, raw, key).toString('base64')
  fs.writeFileSync(`${root}/signature.json`, JSON.stringify({algorithm:'ed25519', key_id:'jzg-lab-sub-data-save-v1', signature})+'\n')
} else {
  fs.rmSync(`${root}/signature.json`, {force:true})
}
NODE

version=$(node -p "require('./manifest.source.json').version")
package="dist/sub-data-save-$version-$target.s2plugin"
# Build the archive from an explicit list; never include a stale signature.
rm -f "$package"
if [ "${S2PLUGIN_UNSIGNED:-0}" = 1 ]; then
  (cd "dist/$target" && zip -q -r "../$(basename "$package")" manifest.json bin ui)
else
  (cd "dist/$target" && zip -q -r "../$(basename "$package")" manifest.json signature.json bin ui)
fi
unzip -t "$package"
printf '%s\n' "$package"
