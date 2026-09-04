# Dependency and toolchain policy

The release toolchain is recorded in `.toolchain.json`. Go, Node.js, npm and
container base image references must agree with that file; run
`python scripts/check-toolchain.py` after changing any of them.

The release candidate freezes dependency versions. Security fixes remain
eligible during the freeze, but they must pass the full release checks. Normal
minor and patch updates are grouped into a monthly review. Major updates are
manual changes and require compatibility, benchmark and deployment evidence.

The production binary does not require Node.js. Node.js and npm are build-time
dependencies only. The pinned release toolchain and the separately tested
Node.js 22 compatibility lane are both recorded in `.toolchain.json`; neither
lane is allowed to silently float to the newest release.
