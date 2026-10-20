#!/bin/bash
# Creates a throwaway CA and server certificate (shared with the test runner
# through /certs), registers the test accounts, then starts Prosody.
set -euo pipefail

CERTS=/certs
mkdir -p "$CERTS" /etc/prosody/certs
if [[ ! -f "$CERTS/ca.crt" ]]; then
	openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=mautrix-xmpp test CA" \
		-keyout "$CERTS/ca.key" -out "$CERTS/ca.crt" 2>/dev/null
	openssl req -newkey rsa:2048 -nodes -subj "/CN=xmpp.test" \
		-keyout "$CERTS/xmpp.test.key" -out "$CERTS/xmpp.test.csr" 2>/dev/null
	printf 'subjectAltName=DNS:xmpp.test,DNS:*.xmpp.test\nextendedKeyUsage=serverAuth,clientAuth\n' > "$CERTS/ext.cnf"
	openssl x509 -req -in "$CERTS/xmpp.test.csr" -CA "$CERTS/ca.crt" -CAkey "$CERTS/ca.key" \
		-CAcreateserial -days 30 -extfile "$CERTS/ext.cnf" -out "$CERTS/xmpp.test.crt" 2>/dev/null
	chmod 0644 "$CERTS"/*
fi
for name in xmpp.test conference.xmpp.test upload.xmpp.test; do
	cp "$CERTS/xmpp.test.crt" "/etc/prosody/certs/$name.crt"
	cp "$CERTS/xmpp.test.key" "/etc/prosody/certs/$name.key"
done
chown -R prosody:prosody /etc/prosody/certs /var/lib/prosody
chmod 0600 /etc/prosody/certs/*.key

for user in ${TEST_USERS:-alice bob carol}; do
	prosodyctl register "$user" xmpp.test "${user}-password" >/dev/null 2>&1 || true
done

exec runuser -u prosody -- prosody -F
