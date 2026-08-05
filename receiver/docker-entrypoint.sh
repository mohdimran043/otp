#!/bin/sh
# Start the API and nginx in one container, and make sure neither outlives the other.
#
# Two processes in one image is usually the wrong shape, and here it is the right one: the proxy exists to be
# the only route to this particular API, so they are one unit — scaling, restarting, or rolling back either
# alone makes no sense. What matters is that a crash of either takes the container down, so an orchestrator
# sees a failure rather than a container that is up and serving nothing.
set -eu

terminate() {
    kill -TERM "$api_pid" 2>/dev/null || true
    kill -TERM "$nginx_pid" 2>/dev/null || true
    wait 2>/dev/null || true
    exit 0
}
trap terminate TERM INT

# Migrations run before anything serves. Doing it here rather than inside the API's own startup means a
# deployment can point this at `-migrate` alone as its own step, and the advisory lock inside makes running
# it on several replicas at once safe either way.
receiver -migrate

receiver &
api_pid=$!

nginx -g 'daemon off;' &
nginx_pid=$!

# Whichever exits first ends the container.
wait -n "$api_pid" "$nginx_pid"
status=$?
terminate
exit "$status"
