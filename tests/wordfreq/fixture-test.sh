#!/bin/sh
set -eu

mode=${1:-full}
go build -o wordfreq .
case "$mode" in
tokenize)
	out=$(printf 'Hello world hello.\n' | ./wordfreq tokenize)
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 3 ]
	printf '%s\n' "$out" | grep -qx hello
	printf '%s\n' "$out" | grep -qx world
	;;
count)
	out=$(printf 'hello world hello\n' | ./wordfreq count)
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 2 ]
	printf '%s\n' "$out" | grep -qx 'hello 2'
	printf '%s\n' "$out" | grep -qx 'world 1'
	;;
full)
	out=$(printf 'Hello world hello.\n' | ./wordfreq)
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 2 ]
	printf '%s\n' "$out" | grep -qx 'hello 2'
	printf '%s\n' "$out" | grep -qx 'world 1'
	;;
*)
	echo "unknown mode $mode" >&2
	exit 2
	;;
esac
