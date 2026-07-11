# Decoder Core Source

SPDX-License-Identifier: GPL-3.0-or-later

This directory is adapted from the offline decompilation core in
[`biggerstar/wedecode`](https://github.com/biggerstar/wedecode), tag `v0.10.6`,
commit `9bf626224511a42f77e4b682756472e06953854e`.

The port removes TypeScript-only syntax and the upstream path aliases, and
uses explicit ESM `.js` imports. It excludes the wedecode CLI, UI, workspace,
package scanning, online app information lookup, wxapkg decryption/unpacking,
and the top-level controller. Logging is routed through ordinary `console`
output because this project owns the CLI presentation layer. The original
`vm2` dependency is replaced by a small `node:vm` compatibility layer; it runs
only inside the project's restricted read-only child process and honors
`WXAPKG_VM_TIMEOUT_MS`. Core file helpers enforce controller-provided read
roots and record writes in an in-memory overlay. The trusted parent process
validates and applies the resulting manifest.

`polyfill/@babel/runtime/helpers/typeof.js` is separately licensed under MIT;
see the root `THIRD_PARTY_NOTICES.md` and `LICENSES/BABEL-MIT.txt`.

`utils/decompile-wxml.js` retains the upstream notice that the implementation
was adapted from `qwerty472123/wxappUnpacker`.
