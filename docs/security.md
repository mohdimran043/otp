# Security

What is encrypted, what is not, and what an air gap does and does not buy you.

## Encrypting a transfer

An optical channel is a broadcast, not a wire. Anything with line of sight to the monitor receives every
frame the display draws, and the protocol is documented, so a second camera in the room decodes a file
exactly as well as the receiver does. An air gap keeps a transfer off the network; it does nothing about who
else is looking at the screen. Encryption is what makes the channel confidential rather than merely
inconvenient to intercept.

Each transfer chooses for itself, in the New Transfer form or the equivalent API fields: **none** by
default, **AES-256-GCM**, or **ChaCha20-Poly1305**. The cipher ID travels in every frame's header, so the
receiver knows what it is looking at without being told out of band — but the payload itself is unreadable
without the key.

The key's path is deliberately manual. It is generated in the sender's form (or typed in by hand), and it is
the operator's job to carry it to the receiver's **Settings** page — over a phone call, a password manager,
a second air-gapped channel, anything that is not the optical one. No API returns key material once it is
stored, and the key itself never crosses the light.

**The honest caveat.** A manifest's filename, content hashes, and callback URL are not part of the encrypted
payload and stay readable to anyone watching the display — the receiver needs them before it has the key, to
know what it is assembling, to verify it afterwards, and to know where to deliver it. And encryption protects
the optical channel specifically: the sender's own database holds the uploaded file in plaintext regardless,
because it has to, to render the frames in the first place. Encrypting a transfer answers "who else can see
the screen," not "who can reach the sender."

## Security notes

- **Neither API authenticates yet, and both now have destructive endpoints.** Anyone who can reach the
  sender can upload a file, change the geometry, or `DELETE` a transfer and every frame it produced; anyone
  who can reach the receiver can start a camera or `DELETE` a received transmission, including a merged file
  that has not been downloaded yet. There is no undo and no soft delete on either side. This was always the
  case for the geometry; deletion makes the consequence of leaving these open considerably worse. Put
  authentication in front of them before exposing either.
- **Both secrets have no defaults.** A default signing secret is not a secret, and the acknowledgement
  channel is the one input the sender takes from outside itself.
- **Callback URLs are allowlisted, redirects not followed.** The URL crosses the gap from outside the
  receiver's trust boundary; without a list the receiver would be a request-forgery proxy.
- **Nothing unverified is delivered or served.** A merged file failing its hash is kept as evidence and
  refused.
- **A transferred file is never served as something a browser executes.** One allowlist
  ([`shared/mediatype`](shared/mediatype/mediatype.go)) governs both ends. **SVG is excluded** — it looks
  like an image and is an XML document that may carry `<script>`.
- **Decompression is bounded by the manifest's declared size.** Every codec here can express a small input
  that expands without limit.
- **Per-transfer keys never cross the optical channel and are never returned by any API.** A key is
  generated on the sender and carried to the receiver's Settings by the operator, out of band; once stored,
  no endpoint on either side echoes it back — `GET`ting a transfer or a keyring entry shows that a key
  exists, never what it is.
