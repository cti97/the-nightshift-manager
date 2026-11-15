#!/bin/sh
# Fail if the working tree contains iCloud-style duplicate copies of source files.
#
# Why this exists: this repo lives inside an iCloud-synced Documents folder, and
# the sync service drops copies named "name 2.go", "go 2.mod", "Makefile 2" into
# the tree. They are untracked, so `git status` says almost nothing, but the Go
# toolchain compiles them: the build dies with "redeclared in this block" and the
# real cause is invisible. It has broken a green build twice.
#
# Fixing the symptom is a `mv`; making it visible is this script.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

found=$(find . \
	-path ./.git -prune -o \
	-path ./.superpowers -prune -o \
	-path ./dist -prune -o \
	-path ./dist.new -prune -o \
	-path ./dist.old -prune -o \
	-path ./bin -prune -o \
	-type f \( -name '* [0-9].*' -o -name '* [0-9]' \) -print | sort)

if [ -n "$found" ]; then
	printf '%s\n' "duplicate copies in the tree — iCloud sync artifacts, not source:" >&2
	printf '%s\n' "$found" >&2
	printf '\n%s\n' "Move them out before building. The build error this causes is" \
		'"redeclared in this block", which names none of the files above.' >&2
	exit 1
fi
