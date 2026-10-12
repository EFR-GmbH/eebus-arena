# Development and code review

The development image contains Ruff 0.16.10, clang-format 14, google-java-format
1.28.0, Go 1.27.1, JDK 21, Gradle 8.5, Cppcheck, Git, and the C build dependencies.
Use the same image locally and in CI. Python and Docker are enough to build it;
preparing upstream overlays also requires Git on the host.

```console
python3 scripts/dev.py build
python3 scripts/dev.py format
python3 scripts/dev.py check
python3 scripts/arena.py prepare
python3 scripts/dev.py test
python3 scripts/dev.py native
```

`format` changes first-party Python, C (including `.c.inc` hooks), Java, and Go.
`check` verifies formatting, runs Ruff's correctness/import/bugbear/modernization
rules, and checks C with Cppcheck. `test` runs Python regression tests and Go vet
and race tests in both the root module and prepared Enbility adapter. `native`
compiles OpenEEBUS and jEEBUS with the pinned upstream sources, tests C child
cleanup without signaling real processes, verifies Gradle dependencies, and
compiles first-party Java with `-Xlint:all -Werror` before running its snapshot
regression test. These are code checks, not live interoperability
qualification.

The wrapper runs as the host UID/GID. Checks mount source read-only; formatter
runs mount it writable. Tool caches live in `.arena/dev-cache`. The toolbox has
no Docker socket mount. Native builds use temporary directories, so Gradle does
not contaminate prepared release inputs with build outputs. Re-run `prepare`
after changing adapters or overlays before testing their generated copies.

## VS Code devcontainer

Install VS Code's Dev Containers extension, open this repository, and run
**Dev Containers: Reopen in Container**. The configuration builds the same
`docker/dev.Dockerfile` used by the command-line workflow.

The image defaults to the non-root `vscode` user. Both the container processes
and the VS Code server, terminals, tasks, and lifecycle commands use that user.
`updateRemoteUserUID` matches its UID/GID to your local account on Linux, so files
created in the mounted checkout belong to you on the host, using
[VS Code's UID/GID mapping](https://code.visualstudio.com/remote/advancedcontainers/add-nonroot-user).
Rebuild the container
if the host account changes. There is no startup command that runs as root or
changes ownership of the checkout.

Editor and normal tool caches stay in `/home/vscode`, outside the mounted
repository; native checks can also use temporary directories.
Python, Go, C, and Java language extensions are configured; Python and C use the
image's pinned formatters. Use `python3 scripts/dev.py format` for Java's pinned
google-java-format and for formatting the whole project. The same `check`,
`test`, and `native` commands run directly inside the devcontainer.

## Preparation recipes

Each `adapters/<stack>/prepare.py` defines `SOURCES` and a normal
`prepare(target, root, lock)` function. The coordinator copies upstream sources,
invokes that function, and writes provenance. Importing a recipe does not download
or edit anything. There is no execution of Python text with injected globals.

Native edits are unified diffs in `adapters/<stack>/patches/`. Their sibling JSON
files record SHA-256 hashes of every input file; `null` denotes a new file. The
patch paths and manifest must agree. Preparation checks all preimages and runs
`git apply --check` before writing any patch hunks. This deliberately rejects
upstream drift, including new copies of an old patch context. Do not work around
failures with fuzzy matching or blanket replacement.

OpenEEBUS has separate baseline and pairing overlays because the two pinned
source revisions differ. Its `.c.inc` hooks need private example structs and
therefore compile inside those example translation units. jEEBUS extracts only
the two instrumented classes from the checksum-verified SPINE source JAR, then
applies its patch. Protocol decisions stay in upstream implementations.

To update an overlay:

1. Prepare the current pinned sources and retain a clean copy of the files named
   in the preimage manifest. For jEEBUS, include the two extracted SPINE classes.
2. Edit and format the native source in a disposable tree. Keep unrelated upstream
   formatting out of the diff; format added code and changed regions.
3. Regenerate the unified diff with `a/` and `b/` paths relative to the generated
   stack directory, and update input hashes only after reviewing upstream changes.
4. Prepare from clean inputs, run the language checks and native builds, and review
   generated `provenance.json`. Never edit a patch without checking it applies.

An alternate state directory keeps experiments separate:

```console
python3 scripts/prepare.py --state /path/to/state --stack openeebus
```

That directory must contain the fetched `sources/` and, for offline Java builds,
the verified source JAR in `cache/`. The ordinary preparation command honors the
same `ARENA_LOCK` override as before. CI also prepares and compiles
`recipes/variants/openeebus-pairing.json`.

## Build-artifact cleanup

After builds and checks finish, preview with `python3 scripts/arena.py clean --dry-run`.
Run `python3 scripts/arena.py clean` to remove prepared source trees and local
binary/output directories. `--cache` additionally clears downloaded source
archives, extracted inputs, `.arena/dev-cache` and `web/node_modules`; the next
build or dependency install recreates them. Run `prepare` again before native
development checks. Runtime identities, evidence, catalogs, installed images and
committed UI assets are preserved.

`--docker-cache` also prunes unused cache in the current Docker builder. That cache
is shared with other projects. `clean --cache --docker-cache --dry-run` previews
the combined cleanup without contacting Docker or removing files.

## Review findings addressed

The original Python lint pass reported 199 issues. Most reflected code compressed
onto single lines, import problems, and recipe globals supplied only by `exec`.
Formatting exposed larger design issues that a formatter alone cannot repair:

- Preparation mixed orchestration with substantial C, Go, Java, and Gradle source
  strings. Native diffs now show edits beside their actual upstream context.
- Bootstrap command parsing also contained archive import/export and source-pin
  implementations. Those operations now have named functions; image validation
  still finishes before either catalog is published.
- Network test callbacks were nested in a loop and captured changing run IDs.
  Each network mode now has its own function scope.
- The C entry point mixed startup, process ownership, command validation, and
  periodic observations. Those responsibilities are separate functions. Child
  cleanup checks PIDs before signaling; asynchronous writes check request-ID
  allocation before passing ownership to the native callback.
- Java startup mixed certificate conversion, native listeners, snapshots, and
  command handling. These are separate methods with explicit imports. Typed
  use-case maps and atomic updates prevent callbacks from overwriting each
  other's fields. Published snapshot maps are not subsequently mutated.
- Go's long composite literals and calls obscured protocol fields and arguments,
  even after gofmt. Those expressions are split before formatting. Comments
  explain lock boundaries, persistence ordering, and observation ownership.

Keep comments focused on constraints a reader cannot infer from the next line:
why stdout is reserved, why callbacks must not hold a lock during native calls,
why persistence syncs a directory, and why suppressed observations are reported.
Avoid narrating assignments or hiding a long expression behind a vague helper.

## Remaining review boundaries

This cleanup does not certify compliance with an organization's internal language
review. Cppcheck and compiler checks are not a substitute for a complete memory
and concurrency audit of the native stacks. The upstream Gradle build still
reports its existing split-package and deprecation warnings; first-party Java
is checked separately with warnings treated as errors. End-to-end network,
pairing, fault, and physical-device qualification still uses the integration
scripts against a running controller.
