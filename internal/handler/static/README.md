# Bundled assets

Everything here is embedded into the binary by `//go:embed static` in
`internal/handler/static.go` and served from `/static/`, which is why the CSP needs
no allowance beyond `'self'`.

`.md` is deliberately absent from the content-type allowlist in `static.go`, so this
file is embedded but not reachable over HTTP.

## htmx 2.0.10

- Source: `https://registry.npmjs.org/htmx.org/-/htmx.org-2.0.10.tgz`, `package/dist/htmx.min.js`
- Licence: Zero-Clause BSD (`htmx.LICENSE`)
- Size: 51 238 bytes
- SHA-256: `71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de`

The bytes were checked against the npm tarball's published `integrity` hash
(`sha512-kdeJe7ZVwaS6QMz/…`) rather than taken from a CDN on trust.

To upgrade, verify the same way — the point of vendoring is lost if the replacement
arrives unchecked:

```sh
V=2.0.11
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

## The htmx indicator rules live in styles.css

htmx injects three `.htmx-indicator` rules as a `<style>` block as soon as it loads,
which `style-src 'self'` blocks — silently, apart from a console warning, and it fails
*open*: the rule that hides an indicator never applies, so a spinner sits there
permanently visible.

So the injection is turned off (`{"includeIndicatorStyles":false}` in the
`htmx-config` meta tag in `templates/partials/document.html`) and the rules live at the
bottom of `styles.css` instead. The two go together: keep both, or drop both.
