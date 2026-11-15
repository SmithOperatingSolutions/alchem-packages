# Alchem packages

Libraries for [Alchem](https://github.com/SmithOperatingSolutions/alchem) that are built on the language and its library rather than inside the compiler, and are released on their own schedule.

| Package | Module | What it is |
| --- | --- | --- |
| [disk_store](disk_store) | `DiskStore` | Text kept under text keys in one file, written through to the disk, read in key order |
| [option_parser](option_parser) | `OptionParser` | Command-line switches read from arguments, written back, and a line split as a shell does |
| [password](password) | `Password` | What to store in place of a password, and checking one against it |
| [version](version) | `Version` | Semantic versions: parsing, comparing, and matching requirements such as `~> 2.1` |
| [websocket](websocket) | `WebSocket` | WebSockets for `HTTP.serve` handlers and clients, with compression |

## Using a package

Import a file by the repository, a version and its path:

```elixir
defmodule Main do
  import "github.com/SmithOperatingSolutions/alchem-packages@v0.2.0/password/password.alc"

  def main() do
    stored = Password.hash("correct horse")
    IO.inspect(Password.verify("correct horse", stored))
  end
end
```

```bash
alchem run main.alc
```

The first build fetches the package and records its content in `alchem.lock` beside the program; commit that file. One version tag covers every package here, and a program uses one version of the repository.

Read a package's documentation from the command line:

```bash
alchem doc github.com/SmithOperatingSolutions/alchem-packages@v0.2.0/disk_store/disk_store.alc
```

Inside a project that imports it, `alchem doc DiskStore`, `alchem doc DiskStore.update` and `alchem doc --search text` find it too.

Each file says the oldest Alchem release it works with, as `require "alchem" "v0.3.8"`: v0.3.8 for `disk_store` and `password`, and v0.3.10 for `option_parser`, `version` and `websocket`.

## Moving from the library

`DiskStore`, `Crypto.hash_password`, `Crypto.verify_password`, `Version`, `OptionParser` and `WebSocket` were part of Alchem's library up to v0.3, and are dropped from it in v0.4.0. Until then a program that imports one of these modules uses the package, and one that does not uses the library's.

- **DiskStore**: add the import. The functions and their names are the same, and a file written by either is read by both. The type of a store is `DiskStore.Store` where it was `disk_store`.
- **Passwords**: add the import and call `Password.hash` and `Password.verify`. The stored text is the same, so passwords stored before still verify.
- **Version**: add the import. The functions, the `Version` and `Version.Requirement` types and the messages are the same.
- **OptionParser**: add the import. The functions are the same; the arguments are a list, as `System.argv()` is, where the library took any enumerable.
- **WebSocket**: add the import. The functions and their names are the same, and the type of a socket is `WebSocket.Socket` where it was `websocket`. Three things differ, each because the package is built on a TCP socket of the library:
  - When the process that owns a socket ends, the connection is dropped without a closing handshake, so the other end sees code 1006 where the library sent 1001. A handler that wants the other end told calls `WebSocket.close` before it returns.
  - Pings and the closing handshake are answered while the socket is being read, by `WebSocket.recv` or an active socket, not between reads.
  - `WebSocket.setopts(ws, [active: ...])` sends events to the process that calls it, and `WebSocket.controlling_process` takes a `pid[WebSocket.Event]`; `ws.conn` is the TCP socket, for `TCP.controlling_process` with any other process.

## What belongs here

A module belongs in a package when it can be built from what the language and its library already offer: Alchem code, and Go code reached through [`extern`](https://github.com/SmithOperatingSolutions/alchem/blob/main/docs/interop.md). What a package cannot build for itself, such as syntax, types the compiler knows, or code the compiler generates for each type, belongs in the compiler; ask for it [there](https://github.com/SmithOperatingSolutions/alchem/issues).

## Layout

Each package is a directory:

```
disk_store/
  disk_store.alc        the module people import
  native.alc            extern declarations, when the package has Go code
  native/store.go       that Go code
  disk_store_test.alc   its tests, which also run the examples in its docs
```

- Module names are shared by a whole program, so a package's module names must be its own.
- Every public function has a `@doc` with at least one `alx>` example where an example can run, and the test file says `doctest Module`.
- Go code uses only the standard library unless the package declares what it needs with `require`. A Go type whose handle is declared `extern shared type` must be safe to use from several goroutines at once.

## Development

You need Go 1.27 or newer and the compiler:

```bash
go install github.com/SmithOperatingSolutions/alchem/cmd/alchem@main
```

```bash
alchem test .
```

```bash
alchem fmt --check .
```

CI runs these on Linux, macOS and Windows, on every change and every night, against the compiler's main branch, so that a change to the compiler that breaks a package is seen before either is released, and against the latest release, with the packages that release can build.

## License

Apache License 2.0; see [LICENSE](LICENSE).
