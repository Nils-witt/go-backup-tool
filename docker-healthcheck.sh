#!/bin/sh
# The image's HEALTHCHECK. Runs `go-backup-tool health`, which asks the
# running instance's GET /api/health whether it is up and its state db is
# reachable (exit 1 when not).
#
# The web UI is optional, so there may be nothing to probe: health exits 2
# when it can't find the instance (no -url, no GO_BACKUP_TOOL_URL, and
# webui.listen unset in ./config.yaml). That counts as healthy here, so a
# container without the web UI isn't flagged unhealthy and restarted by
# autoheal. If the config file lives elsewhere than /data/config.yaml, set
# GO_BACKUP_TOOL_URL (e.g. http://localhost:8080) on the container.
set -u

/usr/local/bin/go-backup-tool health -timeout 5s >/dev/null
rc=$?

if [ "$rc" -eq 2 ]; then
	exit 0
fi

exit "$rc"
