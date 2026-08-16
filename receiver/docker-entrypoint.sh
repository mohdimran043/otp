#!/bin/sh
# Migrate, then serve.
#
# nginx used to run beside the API in this container and now has its own, so this starts one process and
# the shell's job is only to make sure migrations finish first. The proxy is still the only route in from
# the host — the API's port is not published — but it is now a container that can be restarted,
# reconfigured or upgraded without touching this one, which is what a proxy setting change should cost.
set -eu

# Migrations run before anything serves. Doing it here rather than inside the API's own startup means a
# deployment can point this at `-migrate` alone as its own step, and the advisory lock inside makes running
# it on several replicas at once safe either way.
receiver -migrate

# exec, so the API is PID 1 and receives the orchestrator's signals directly. With nginx gone there is
# nothing left for a shell to supervise, and a shell in the middle would swallow SIGTERM and turn every
# stop into the ten-second kill.
exec receiver "$@"
