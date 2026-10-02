Vendored, unmodified except where noted:

- preact 10.29.8 (`dist/preact.module.js`, `hooks/dist/hooks.module.js`), MIT, from `npm pack preact@10.29.8`
  (integrity sha512-ej2aVZ+vZ8WO7tvlQWRM9N63A0KzF9q4mWJfDUHgYaIofWY9hu74QdnQrjoPMmZi2/nZ5gN0bJCQF49xQqx09Q==)
- htm 3.1.1 (`dist/htm.module.js`), Apache-2.0, from `npm pack htm@3.1.1`
  (integrity sha512-983Vyg8NwUE7JkZ6NmOqpCZ+sh1bKv2iYTlUkzlWmA5JD2acKoxd4KVxbMmxX/85mtfdnDmTFoNKcg5DGAvxNQ==)

Change: `hooks.module.js` imports `./preact.module.js` instead of the bare `preact`, so no
inline import map is needed and the page can run under `script-src 'self'`.
