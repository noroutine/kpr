#!/bin/bash
# Stage dev TLS material into dev/tls (readable copies for the
# non-root container user). Rerun after deleting a fixture.
# Idempotent: generated fixtures persist until deleted
# (rotating the dev identity every boot would churn local
# trust for nothing).
set -euo pipefail

cd "$(dirname "$0")/.."

mkdir -p dev/tls

# Local dev identity: self-signed, served by the dev stack's
# HTTPS listeners and filed as the `local` bundle. The SAN list
# covers every name the stack answers to — Go clients reject
# CN-only certificates, and a browser opening localhost must
# see its own name as much as curl reaching the service name.
if [ ! -f dev/tls/local-cert.pem ]; then
  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout dev/tls/local-key.pem -out dev/tls/local-cert.pem \
    -days 825 -subj "/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,DNS:kpr,IP:127.0.0.1"
  chmod 644 dev/tls/local-cert.pem dev/tls/local-key.pem
  echo "dev-certs: issued local self-signed (SANs localhost, kpr, 127.0.0.1)"
else
  echo "dev-certs: kept local-cert.pem"
fi

# Mozilla roots for the trust-only bundle: public data, staged
# rather than committed to keep the repo lean. Refreshed every
# run — `up` rebuilds staging, so the dev trust stays current.
# Offline keeps the last copy with a warning instead of failing
# the boot; no copy at all fails loudly, since the filed bundle
# names it.
if curl -sS --fail https://curl.se/ca/cacert.pem -o dev/tls/.mozilla-ca.tmp; then
  mv dev/tls/.mozilla-ca.tmp dev/tls/mozilla-ca.pem
  chmod 644 dev/tls/mozilla-ca.pem
  echo "dev-certs: refreshed mozilla roots"
else
  rm -f dev/tls/.mozilla-ca.tmp
  if [ -f dev/tls/mozilla-ca.pem ]; then
    echo "dev-certs: offline, keeping staged mozilla roots" >&2
  else
    echo "dev-certs: no mozilla roots and offline — trust bundle will refuse" >&2
    exit 1
  fi
fi

# Expired identity proving the expiry path: valid two days
# ago, dead yesterday — cards voice expired, not broken.
# (openssl has no backdated issuance; python does.)
python3 -c "import cryptography" 2>/dev/null || { echo "dev-certs: python3 cryptography needed for the expired-cert fixture (pip install cryptography)" >&2; exit 1; }
if [ ! -f dev/tls/expired-cert.pem ]; then
  python3 - <<'PYEOF'
from cryptography import x509
from cryptography.x509.oid import NameOID
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from datetime import datetime, timedelta, timezone
now = datetime.now(timezone.utc)
key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "expired.test")])
cert = (x509.CertificateBuilder()
    .subject_name(name).issuer_name(name).public_key(key.public_key())
    .serial_number(x509.random_serial_number())
    .not_valid_before(now - timedelta(days=2)).not_valid_after(now - timedelta(days=1))
    .add_extension(x509.SubjectAlternativeName([x509.DNSName("expired.test")]), critical=False)
    .sign(key, hashes.SHA256()))
with open("dev/tls/expired-cert.pem", "wb") as f:
    f.write(cert.public_bytes(serialization.Encoding.PEM))
with open("dev/tls/expired-key.pem", "wb") as f:
    f.write(key.private_bytes(serialization.Encoding.PEM,
        serialization.PrivateFormat.TraditionalOpenSSL,
        serialization.NoEncryption()))
PYEOF
  chmod 644 dev/tls/expired-cert.pem dev/tls/expired-key.pem
  echo "dev-certs: issued expired fixture (dead yesterday)"
else
  echo "dev-certs: kept expired-cert.pem"
fi
