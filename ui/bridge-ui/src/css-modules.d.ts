// CSS modules under src/ are copied into dist/ by scripts/copy-css.mjs after tsc
// runs, so a consumer's bundler resolves `./X.module.css` next to `X.js` exactly
// as it would in a source tree. tsc itself only needs to know the import's shape.
declare module '*.module.css' {
  const classes: { readonly [className: string]: string }
  export default classes
}
