# deploy — the proxy, TLS, and the admin consoles

Both applications run behind nginx, and nginx runs in a container of its own on each side. The API's port is
not published, so from the host the proxy is the only way in. What is here is what the two proxies share, and
what a production deployment adds.

## Why nginx is its own container

It used to run beside the API in one container, on the argument that the proxy exists to be the only route to
that particular API and so the two are one unit. That argument is fair and it lost to an operational one:
every change to a timeout, a header, a certificate or an admin path rebuilt and redeployed the *application*
image in order to alter a file the application never reads. In production that is the difference between
restarting a proxy and rolling the service.

So the configuration is mounted rather than baked:

```bash
docker compose restart nginx
```

is the whole cost of a proxy change — measured at about a third of a second, with no image build. The three
mounted files are `nginx.conf`, `../deploy/nginx-admin.conf`, and whatever is in `./tls`.

The built browser app stays baked into the proxy image, because that genuinely is part of a release: the
assets and the API that serves them are versioned together, and a mismatched pair is a broken page rather
than a configuration difference. Both targets live in one Dockerfile — `target: app` and `target: proxy` — so
the web build runs once and is shared.

Ports are unchanged: 1000 for the sender, 2000 for the receiver. What moved is which container listens
behind them.

## One origin

Everything a deployment exposes is on one address per side:

| Path | Sender (`:1000`) | Receiver (`:2000`) |
|---|---|---|
| `/` | the app | the app |
| `/api/…` | the API | the API |
| `/minio/` | MinIO console | MinIO console |
| `/pgadmin/` | pgAdmin | pgAdmin |

No extra published ports. Postgres has never been published and still is not — pgAdmin reaches it over the
compose network — and MinIO's console is reachable only through the proxy. That is the point of putting them
here rather than on ports of their own: an object-store console on 9001 with its own credentials is a second
front door that nobody remembers to firewall.

Neither console is authenticated by nginx; they carry their own logins. So these paths are exactly as exposed
as the application is. A deployment reachable from the internet should either drop the two services or put an
authenticating proxy in front of the whole origin.

### Credentials

pgAdmin: `admin@example.com` / `otp-dev-password` by default. Override with `OTP_SENDER_PGADMIN_EMAIL` /
`OTP_SENDER_PGADMIN_PASSWORD` (and the `OTP_RECEIVER_*` equivalents). The address must have a real TLD —
pgAdmin validates it even with deliverability checking off, and rejects things like `.local` by restarting
forever with the reason on stdout and nothing on the page.

The database server is pre-registered, so the console opens on this deployment's own database rather than on
an empty list. Note that the receiver's database is also named `otp_sender`: both compose files set it that
way, it is harmless, and `pgadmin-servers.json` names it as it actually is rather than as it reads.

MinIO: the root user and password from `OTP_*_MINIO_ACCESS_KEY` / `OTP_*_MINIO_SECRET_KEY`.

`MINIO_BROWSER_REDIRECT_URL` has to match the origin the console is actually reached at, because the console
writes it into its own links. A deployment reachable at more than one origin — a local port and a tunnel, say
— can only name one; set `OTP_*_MINIO_BROWSER_URL` to whichever people actually use. The console works from
either; what breaks on the other is its redirects.

## TLS

Development runs on plain HTTP with no edits and nothing commented out. The proxies end with:

```nginx
include /etc/nginx/tls/*.conf;
```

which matches nothing when the directory is empty — not an error in nginx. `./tls` is mounted read-only into
each container, so enabling HTTPS is a matter of putting files in it rather than editing a config the next
image build would overwrite:

1. Put `fullchain.pem` and `privkey.pem` in `sender/tls/` (or `receiver/tls/`).
2. Copy `deploy/tls/https.conf.example` to `https.conf` in the same directory, and set `server_name`.
   For the receiver, change the four `proxy_pass http://sender_api` lines to `receiver_api`.
3. Publish 443 in that side's `docker-compose.yml`.
4. `docker compose up -d`. Check it first with `docker exec <container> nginx -t`, which reports a bad
   certificate path before a restart does.

The applications need no change: they already trust `X-Forwarded-Proto`, so they report https addresses as
soon as the proxy terminates TLS.

The example is a complete, valid server block — it has been checked against a real nginx with a real
certificate, not merely written. Two things in it are deliberately left off:

- **HSTS** is a promise a browser remembers for months. Committing to it from an example, on a deployment
  that may be reached by IP or share a hostname with something served over HTTP, is how a working system
  becomes unreachable. Add it once the hostname is settled.
- **The HTTP→HTTPS redirect** is commented out, because enabling it changes what the existing port 80 does.
  A health probe, a sibling container, or a tunnel terminating its own TLS and forwarding plain HTTP would
  all break the moment the file lands. Turn it on deliberately, after checking what actually talks to port 80.

Certificates are gitignored; the `.gitkeep` files are not, because the directory has to exist for the mount
and the glob.

## Why the resolver line

The admin backends are separate containers, so nginx resolves them by name — but through a variable and
Docker's embedded DNS at request time, rather than in an `upstream` block. A static name is resolved once at
config load and a missing one is fatal, which would mean a stopped pgAdmin takes the whole application down
with it. As written, a deployment that does not run the admin services loses those two paths and nothing
else.
