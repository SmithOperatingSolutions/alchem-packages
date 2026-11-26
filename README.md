# Alchem packages

Libraries for [Alchem](https://github.com/SmithOperatingSolutions/alchem) that are built on the language and its library rather than inside the compiler, and are released on their own schedule.

| Package | Module | What it is |
| --- | --- | --- |
| [deflate](deflate) | `Deflate` | DEFLATE compression, the raw stream inside gzip, zip, PNG and permessage-deflate |
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

Each file says the oldest Alchem release it works with, as `require "alchem" "v0.4.2"`: v0.3.8 for `password`, v0.3.10 for `option_parser` and `version`, and v0.4.2 for `deflate`, `disk_store` and `websocket`, which are written for the native compiler and use the `Binary` module, `Crypto.crc32` and `File.sync`. A program on an older release imports `v0.2.0` of these three.

## Moving from the library

`DiskStore`, `Crypto.hash_password`, `Crypto.verify_password`, `Version`, `OptionParser` and `WebSocket` were part of Alchem's library up to v0.3, and are dropped from it in v0.4.0. Until then a program that imports one of these modules uses the package, and one that does not uses the library's.

- **DiskStore**: add the import. The functions and their names are the same, and a file written by either is read by both. The type of a store is `DiskStore.Store` where it was `disk_store`. `DiskStore.update` runs its function in the calling process, and runs it again when another write changes the key in the meantime, so the function should do nothing but compute the new value.
- **Passwords**: add the import and call `Password.hash` and `Password.verify`. The stored text is the same, so passwords stored before still verify.
- **Version**: add the import. The functions, the `Version` and `Version.Requirement` types and the messages are the same.
- **OptionParser**: add the import. The functions are the same; the arguments are a list, as `System.argv()` is, where the library took any enumerable.
- **WebSocket**: add the import. The functions and their names are the same, and the type of a socket is `WebSocket.Socket` where it was `websocket`. Three things differ, each because the package is built on a TCP socket of the library:
  - When the process that owns a socket ends, the connection is dropped without a closing handshake, so the other end sees code 1006 where the library sent 1001. A handler that wants the other end told calls `WebSocket.close` before it returns.
  - Pings and the closing handshake are answered while the socket is being read, by `WebSocket.recv` or an active socket, not between reads.
  - `WebSocket.setopts(ws, [active: ...])` sends events to the process that calls it, and `WebSocket.controlling_process` takes a `pid[WebSocket.Event]`; `ws.conn` is the TCP socket, for `TCP.controlling_process` with any other process.

## What belongs here

A module belongs in a package when it can be written in Alchem with what the language and its library already offer. Packages have no Go code: [`extern`](https://github.com/SmithOperatingSolutions/alchem/blob/main/docs/interop.md) is gone from the language in v0.4.0, whose compiler does not build programs with Go. What a package cannot build for itself, such as syntax, types the compiler knows, or code the compiler generates for each type, belongs in the compiler; ask for it [there](https://github.com/SmithOperatingSolutions/alchem/issues).

## Layout

Each package is a directory:

```
websocket/
  websocket.alc         the module people import
  protocol.alc          other modules it imports, when it has more than one
  websocket_test.alc    its tests, which also run the examples in its docs
```

A package may import another one here by its path, as `websocket` imports `../deflate/deflate.alc`; one version tag covers both.

- Module names are shared by a whole program, so a package's module names must be its own.
- Every public function has a `@doc` with at least one `alx>` example where an example can run, and the test file says `doctest Module`.
- Bytes are read and written with `Binary`. Bytes made one at a time in a tuple, or in a struct that goes through `Enum.reduce`, are kept as a list and made a string once with `Binary.from_list`: the native compiler still copies a string added to there ([alchem#68](https://github.com/SmithOperatingSolutions/alchem/issues/68)).

## Development

You need the compiler, v0.4.2 or newer. Its installer puts the native `alchem` in `~/.local/bin` on macOS and Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/SmithOperatingSolutions/alchem/main/install.sh | sh
```

On Windows, `irm https://raw.githubusercontent.com/SmithOperatingSolutions/alchem/main/install.ps1 | iex` does the same. To try a change to the compiler before it is released, build its main branch with Go 1.27 or newer instead:

```bash
go install github.com/SmithOperatingSolutions/alchem/cmd/alchem@main
```

```bash
alchem test .
```

```bash
alchem fmt --check .
```

CI runs these on Linux, macOS and Windows, on every change and every night: against the compiler's main branch, so that a change to the compiler that breaks a package is seen before either is released; against the native alchem of the latest release, as its installers give it; and against v0.3.10, with the packages that still build there.

## License

Apache License 2.0; see [LICENSE](LICENSE).
