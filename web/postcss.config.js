// Tailwind v4 moved the PostCSS plugin into its own package; `tailwindcss` can no
// longer be used directly as a plugin here. autoprefixer is dropped because v4 does
// vendor prefixing itself via Lightning CSS.
export default {
  plugins: {
    '@tailwindcss/postcss': {},
  },
}
