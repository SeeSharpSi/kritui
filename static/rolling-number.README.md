# Rolling Number vendor files

Vendored from [Rolling Number](https://rolling.kitlangton.dev), repository [kitlangton/rolling-number](https://github.com/kitlangton/rolling-number), package version `0.4.1`, source commit `5acea33e5a16326f887f6c8966c3ddea029257e7`. License: MIT; see `rolling-number.LICENSE.txt`.

This bundle includes DOM-only `createRollingNumber` and `createRollingText` exports (plus `formatValue` and `FLAP_CHARSET`). React and Solid adapters are not included. Files: `rolling-number.min.js` (standalone browser IIFE, global `RollingNumber`) and `rolling-number.min.css` (library styles). Loading the script does not initialize any elements. No application UI behavior is enabled by vendoring these files.

Example, after separately loading stylesheet and script:

```html
<link rel="stylesheet" href="/static/rolling-number.min.css">
<script src="/static/rolling-number.min.js"></script>
<span id="counter">123</span>
<script>
  const controller = RollingNumber.createRollingNumber(
    document.querySelector("#counter"),
    { value: 123 },
  );
</script>
```

## Refresh

Extract source commit `5acea33e5a16326f887f6c8966c3ddea029257e7` outside the repository and set `SOURCE_DIR` to its extracted directory. Run from Kritui repository root. Requires Go and network access for the pinned esbuild module; no Node.js, npm, Bun, or TypeScript installation is needed.

```sh
go run github.com/evanw/esbuild/cmd/esbuild@v0.25.12 "$SOURCE_DIR/src/index.ts" --bundle --minify --format=iife --global-name=RollingNumber --platform=browser --target=es2020 --charset=utf8 --legal-comments=inline --line-limit=500 --banner:js='/*! Rolling Number v0.4.1 | MIT | https://github.com/kitlangton/rolling-number | source 5acea33e5a16326f887f6c8966c3ddea029257e7 | see rolling-number.LICENSE.txt */' --outfile=static/rolling-number.min.js
go run github.com/evanw/esbuild/cmd/esbuild@v0.25.12 "$SOURCE_DIR/src/styles.css" --minify --line-limit=500 --outfile=static/rolling-number.min.css
```

SHA256 for generated files at this revision:

| File | SHA256 |
| --- | --- |
| `rolling-number.min.js` | `1a5f6aca8579e435f6ea8e2dba4cb58e436cb62983b0b23d3edacd7433030080` |
| `rolling-number.min.css` | `0f0e51494f02df7e7709e8fc0d405951c2602b4c22c12fc893d56454c92d6643` |

Source integrity hashes:

| Source | SHA256 |
| --- | --- |
| `src/index.ts` | `65a607291c99a600be5d4c496f878bf4508a7bf89b50f1dfbecda264c64f9e68` |
| `src/styles.css` | `cf1e194ec76ee7dd350396c1497b99c3f299568b1d98b7d18403095b94aecb6b` |
| `LICENSE` | `4994c24b5bbb85529407e8cd0211efcf819236899d2c911cbdb18c00f231273a` |
