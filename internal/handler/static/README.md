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

## Cally 0.9.2

The calendar behind `<date-range-input>`. Loaded by the apply page as its own
`<script type="module">`; the component does not import it (a relative import would
bypass the content-hashed URL).

- Source: `https://registry.npmjs.org/cally/-/cally-0.9.2.tgz`, `package/dist/cally.js`
  (copied from the component repo's `vendor/`, where the tarball hash is recorded)
- Licence: MIT (`cally.LICENSE`)
- Size: 38 355 bytes
- SHA-256: `4dcba6a3b8ec63b66ff2fbfc6cf47b7dee1eb4896eac89fe7487d82e28e15870`

## date-range-input

The `<date-range-input>` web component on the apply page and the admin Record leave form. It
started as our own standalone component (originally taken at commit `33999ff` of a
separate repository) and is now a vendored copy maintained here: edit it in place, there
is no upstream to sync from.

- Licence: MIT (`date-range-input.LICENSE`)
- Size: 32 691 bytes
- SHA-256: `810848d17b3a249e412b0493a0d1ba6bf1679e4d86bc8421365b9def3ab08ecd`

**Local changes:** the `marks` attribute (holiday and booked-day parts via Cally's
`getDayParts`, documented at the top of the file) and the `::part(holiday)` /
`::part(booked*)` styles (plus background-color in the hover rules, and a selected-day
hover rule).

After changing `date-range-input.js` or `cally.js`, record the new size and hash above:

```sh
sha256sum internal/handler/static/date-range-input.js internal/handler/static/cally.js
```

The `--dri-*` custom properties are mapped onto the theme tokens in `styles.css`.

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
