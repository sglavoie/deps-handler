#!/bin/sh
# Runs as root on the Pi, from the directory scripts/deploy-pi.sh copied:
# installs the binaries and units, then restarts both services. Settings
# and keys in /etc/deps-handler-cloud are left as they are.
set -eu
cd "$(dirname "$0")"
id dmc >/dev/null 2>&1 || useradd --system --home-dir /var/lib/deps-handler-cloud --shell /usr/sbin/nologin dmc
install -d -m 755 /opt/deps-handler-cloud
install -d -m 750 -o root -g dmc /etc/deps-handler-cloud
for bin in server admin cloudflared; do
	install -m 755 "$bin" /opt/deps-handler-cloud/"$bin"
done
install -m 755 dmc-admin /usr/local/bin/dmc-admin
install -m 644 deps-handler-cloud.service deps-handler-cloud-tunnel.service /etc/systemd/system/
systemctl daemon-reload
if [ -f /etc/deps-handler-cloud/prod.env ]; then
	systemctl enable deps-handler-cloud deps-handler-cloud-tunnel
	systemctl restart deps-handler-cloud deps-handler-cloud-tunnel
else
	echo "install: no /etc/deps-handler-cloud/prod.env yet: services installed, not started"
fi
