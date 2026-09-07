# Dependency and toolchain policy

The release toolchain is recorded in `.toolchain.json`. Go, Node.js, npm and
container base image references must agree with that file; run
`python scripts/check-toolchain.py` after changing any of them.

The release candidate freezes dependency versions. Security fixes remain
eligible during the freeze, but they must pass the full release checks. Normal
minor and patch updates are grouped into a monthly review. Major updates are
manual changes and require compatibility, benchmark and deployment evidence.

The production binary does not require Node.js. Node.js and npm are build-time
dependencies only. Release artifacts use the exact Go 1.26.7, Node.js 24 and
npm 12 pins recorded in `.toolchain.json`. `go.mod` deliberately declares the
Go 1.26.0 compatibility floor, and CI tests that floor in a separate lane;
release and container jobs remain on the exact release pin. The Dashboard is
likewise tested on the recorded Node.js 22/npm 11 compatibility lane. None of
these lanes is allowed to silently float to the newest release.

The current `quic-go`, gRPC, Vite and TypeScript versions remain explicit
release pins. They are not downgraded merely to widen an environment range:
the compatibility lane proves the supported compiler/runtime floor, while a
dependency change requires its own API, benchmark and deployment evidence.

The Dashboard lockfile may use an npm `overrides` entry when a transitive
dependency needs a security fix before its parent releases a new range. The
current `glob` override is pinned to 10.5.0 to exclude the vulnerable 10.4.x
CLI range; keep the override until the dependency tree no longer needs it.
Run `npm audit --audit-level=high` and the full release check after changing
the lockfile. The final Controller image contains only the compiled binary,
but the build and test toolchain is still part of the trusted release path.
