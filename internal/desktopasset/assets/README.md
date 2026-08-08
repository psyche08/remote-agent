The signed `remote-agent-desktop` binary is placed here by
`deploy/publish-release.sh` immediately before the release build, so the agent
carries it and can write it out on the device.

It is deliberately not checked in: it is a build product, it is platform
specific, and a stale copy in git would be worse than none — the agent would
happily install a helper nobody built from the commit being shipped.

This file exists so the embed directory is present in a fresh checkout and
`go build` works without a helper. A build with no helper reports computer use
as unavailable rather than failing to start.
