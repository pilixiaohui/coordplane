#!/bin/sh
set -eu

mode=${1:-full}
bin="$HOME/wordfreq"
trap 'rm -f "$bin"' EXIT
go build -o "$bin" .
case "$mode" in
tokenize)
	out=$(printf 'Hello world hello.\n' | "$bin" tokenize)
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 3 ]
	printf '%s\n' "$out" | grep -qx hello
	printf '%s\n' "$out" | grep -qx world
	;;
count)
	out=$(printf 'hello world hello\n' | "$bin" count)
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 2 ]
	printf '%s\n' "$out" | grep -qx 'hello 2'
	printf '%s\n' "$out" | grep -qx 'world 1'
	;;
full)
	out=$(printf 'Hello world hello.\n' | "$bin")
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 2 ]
	printf '%s\n' "$out" | grep -qx 'hello 2'
	printf '%s\n' "$out" | grep -qx 'world 1'
	;;
*)
	echo "unknown mode $mode" >&2
	exit 2
	;;
esac
