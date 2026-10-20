#!/bin/sh
set -e
python -m synapse.app.homeserver -c /config/homeserver.yaml --generate-keys
exec python -m synapse.app.homeserver -c /config/homeserver.yaml
