# Bundled assets

Everything here is embedded into the binary by `//go:embed static` in
`internal/handler/static.go` and served from `/static/`, which is why the CSP needs
no allowance beyond `'self'`.

`.md` is deliberately absent from the content-type allowlist in `static.go`, so this
file is embedded but not reachable over HTTP.

## htmx 4.0.0

- Source: `https://registry.npmjs.org/htmx.org/-/htmx.org-4.0.0.tgz`, `package/dist/htmx.min.js`
  (the classic-script build; `htmx.esm.min.js` is the module build and is not used)
- Licence: Zero-Clause BSD (`htmx.LICENSE`)
- Size: 36 716 bytes
- SHA-256: `e484d9171a9db30a39c8f16e3d709d4137f3211c659f8e6125816635033d593f`

The bytes were checked against the npm tarball's published `integrity` hash
(`sha512-T/171FUY93Kdfp8t…`) rather than taken from a CDN on trust.

To upgrade, verify the same way — the point of vendoring is lost if the replacement
arrives unchecked:

```sh
V=4.0.0   # pin it: the npm `latest` tag may still point at 2.x
curl -sSO "https://registry.npmjs.org/htmx.org/-/htmx.org-$V.tgz"
# compare sha512 of the tarball against .dist.integrity from
#   https://registry.npmjs.org/htmx.org/$V
tar -xzOf "htmx.org-$V.tgz" package/dist/htmx.min.js > htmx.min.js
sha256sum htmx.min.js   # record it above
```

Then update the version and hash recorded above. Nothing else needs changing:
templates reference assets as `{{asset "htmx.min.js"}}`, which appends a hash of the
file's own contents to the URL, so replacing the file invalidates every cached copy on
its own.

## htmx 4 and the CSP

Nothing in `styles.css` or the `<head>` configures htmx. htmx 2 injected its
`.htmx-indicator` rules as a `<style>` block, which `style-src 'self'` blocked, so
the rules were copied into `styles.css` and the injection switched off. htmx 4
installs the same rules as a constructable stylesheet (`adoptedStyleSheets`), which
`style-src` does not govern, so the copy and the `htmx-config` meta tag are gone.
If a page ever uses `hx-indicator`, no CSS is needed.

htmx 4 sends requests with `fetch()` (still governed by `connect-src 'self'`), and it
swaps 4xx/5xx responses by default (only 204 and 304 are skipped), so a server error
or an auth response replaces the preview rather than leaving it stale.
