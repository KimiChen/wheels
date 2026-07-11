# Third-Party Notices

## wedecode

Except for the separately licensed Babel helper listed below,
`src/decoder/core/` contains JavaScript code adapted from:

- Project: `biggerstar/wedecode`
- Source: https://github.com/biggerstar/wedecode
- Version: `v0.10.6`
- Commit: `9bf626224511a42f77e4b682756472e06953854e`
- License: `GPL-3.0-or-later`

The surrounding project is distributed under the same `GPL-3.0-or-later`
license. The root `LICENSE` file applies to the adapted code unless a file is
explicitly identified as separately licensed.

Material changes include:

- porting the TypeScript offline decompilation modules to project-local ESM JavaScript;
- removing the upstream CLI, interactive prompts, UI, workspace server, update checks, remote app-information lookup and external command boundary;
- replacing upstream package decryption and extraction with a strictly validated local implementation;
- replacing fatal `process.exit(0)` paths with propagated errors;
- running compiled package-code analysis in a read-only child process with time, memory and Node permission restrictions;
- capturing decompiler writes in memory and applying a strictly validated manifest from the trusted parent process;
- authenticating the worker completion protocol and rejecting unauthenticated or malformed final responses;
- adding output containment, symlink, hard-link, Windows path-alias and unsafe-clear checks;
- normalizing behavior for Windows and POSIX paths.

The WXML recovery module retains wedecode's upstream attribution that portions
were derived from <https://github.com/qwerty472123/wxappUnpacker> and modified
by the wedecode project. That inherited notice does not identify a revision or
separate license, so this project does not infer either one.

## Babel runtime helper

`src/decoder/core/polyfill/@babel/runtime/helpers/typeof.js` contains a helper
vendored through wedecode from `@babel/runtime`:

- Project: Babel
- Source: https://github.com/babel/babel
- Copyright: 2014-present Sebastian McKenzie and other contributors
- License: MIT

The complete license text is in `LICENSES/BABEL-MIT.txt`.
