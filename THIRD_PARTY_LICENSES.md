# Third-Party Licenses

This repository is licensed under the Mozilla Public License 2.0 (see `LICENSE`).

## go-substrate-rpc-client (vendored substrate RPC client)

The vendored `third_party/go-substrate-rpc-client` tree is Apache-2.0; its `gethrpc` package is LGPL-3.0. Its license files and authors are in that tree.

## go-yamux (vendored stream multiplexer)

- **Component**: `github.com/libp2p/go-yamux/v5` v5.1.0.
- **Vendored at**: `third_party/go-yamux/`; `PATCH.md` describes the modification.
- **Licenses**: MPL-2.0 (`third_party/go-yamux/LICENSE`) and BSD
  (`third_party/go-yamux/LICENSE-BSD`).

The Source Code Form of the modified file is in the public source tree at `third_party/go-yamux/session.go` and in the release tag’s source archive.
