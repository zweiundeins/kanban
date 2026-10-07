#!/bin/sh
# First install on the VPS (as root): the user, the binary, the units.
#   scp kanban deploy/* root@host:/tmp/kanban-deploy/ && ssh root@host sh /tmp/kanban-deploy/install.sh
set -eu
here=$(dirname "$0")
id kanban >/dev/null 2>&1 || useradd --system --home /var/lib/kanban --shell /usr/sbin/nologin kanban
install -D -m 0755 "$here/kanban" /opt/kanban/kanban
install -m 0644 "$here/kanban.service" "$here/kanban-backup.service" "$here/kanban-backup.timer" /etc/systemd/system/
systemctl daemon-reload
if [ ! -f /var/lib/kanban/kanban.db ]; then
	install -d -o kanban -g kanban -m 0700 /var/lib/kanban
	runuser -u kanban -- /opt/kanban/kanban seed -db /var/lib/kanban/kanban.db
fi
systemctl enable --now kanban.service kanban-backup.timer
systemctl restart kanban.service
systemctl --no-pager status kanban.service | head -5
