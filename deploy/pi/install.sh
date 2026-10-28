#!/bin/sh
# Runs as root on the Pi, from the directory scripts/deploy-pi.sh copied:
# installs the binaries and units, then restarts both services. Settings
# and keys in /etc/deps-merger-cloud are left as they are.
set -eu
cd "$(dirname "$0")"
id dmc >/dev/null 2>&1 || useradd --system --home-dir /var/lib/deps-merger-cloud --shell /usr/sbin/nologin dmc
install -d -m 755 /opt/deps-merger-cloud
install -d -m 750 -o root -g dmc /etc/deps-merger-cloud
for bin in server admin cloudflared; do
	install -m 755 "$bin" /opt/deps-merger-cloud/"$bin"
done
install -m 755 dmc-admin /usr/local/bin/dmc-admin
install -m 644 deps-merger-cloud.service deps-merger-cloud-tunnel.service /etc/systemd/system/
systemctl daemon-reload
if [ -f /etc/deps-merger-cloud/prod.env ]; then
	systemctl enable deps-merger-cloud deps-merger-cloud-tunnel
	systemctl restart deps-merger-cloud deps-merger-cloud-tunnel
else
	echo "install: no /etc/deps-merger-cloud/prod.env yet: services installed, not started"
fi
