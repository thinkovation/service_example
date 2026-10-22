# Vendored Swagger UI

`swagger-ui.css` and `swagger-ui-bundle.js` are taken unmodified from
[swagger-ui-dist](https://www.npmjs.com/package/swagger-ui-dist) **5.34.0**,
Apache-2.0 (see `LICENSE` and `NOTICE`). `index.html` is ours.

They are vendored, rather than loaded from a CDN when the page opens, so that
`/swagger` works on a machine with no internet access and this service never
serves third-party code from its own origin.

To update, take the two files from a newer release of the package and change
the version above:

```sh
npm pack swagger-ui-dist@<version>
tar xzf swagger-ui-dist-<version>.tgz
cp package/swagger-ui.css package/swagger-ui-bundle.js package/LICENSE package/NOTICE .
```

Only these two assets are needed: `index.html` loads `SwaggerUIBundle` with the
`apis` preset, not the standalone preset with its URL topbar.
