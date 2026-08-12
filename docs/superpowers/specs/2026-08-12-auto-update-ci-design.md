# Automatic Update CI Design

## Scope

Add two post-release checks. One runs on `macos-26`. The other runs inside a
Debian Trixie container. Both verify that Clyde's daemon scheduler replaces an
installed older version with the release that the workflow just published.

## Workflow

The release workflow keeps its existing reusable release job. Two dependent
jobs start only after that job publishes and verifies every release asset. This
ordering prevents either check from observing the previous latest release.

Both jobs check out the triggering commit with tags, install their platform
prerequisites, and run one shared Go command. The Debian job uses an official
`debian:trixie` job container instead of treating Ubuntu as Debian.

## Update Probe

The command resolves the release produced for the triggering commit through the
GitHub API. It selects the preceding release as the installed version identity.
It then builds the checked-in Clyde source with that older clean release stamp
and installs the binary in a temporary directory.

The command starts `clyde daemon run` with temporary XDG roots and a listener-free
configuration. This exercises the production supervisor and update scheduler while
isolating Clyde-owned state, cache, configuration, and sockets. The scheduler waits
normally, discovers the new release, downloads its native archive, verifies its
checksum and GitHub attestations, validates the candidate, and replaces the
installed binary.

The command polls the installed binary until `clyde --version` reports the target
release. It also reads the isolated update state and requires `last_result` to be
`applied`. A timeout prints the daemon output and preserved update state before
failing.

## Safety

The command creates one temporary root and removes it after success or failure.
It starts the daemon in a dedicated process group and terminates the supervisor
and worker together. It never installs into a runner system path. The GitHub
token remains in an environment variable and never appears in command arguments
or output.

## Verification

The two jobs are the behavioral tests. They use the real daemon scheduler and
published release boundary. Local verification covers Go tests, workflow syntax,
focused updater tests, and `make check`.
