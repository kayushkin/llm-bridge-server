#!/usr/bin/env bash
# Score this package's tests by sabotage rather than by asserting they are good.
#
# HOW TO READ THE VERDICT COLUMN. This table scores MECHANISMS: each row reverts
# one piece of the fix and asks whether the suite notices. So here:
#
#     CAUGHT    == good. The suite detects that mechanism's absence.
#     UNNOTICED == a hole, EXCEPT on a row whose expectation IS UNNOTICED, where
#                  it is required and where the row exists to prove the scorer
#                  can still return a negative.
#
# That is the opposite of a table scoring ASSERTIONS by deletion, where UNNOTICED
# means the assertion was the sole detector. Stated here because a verdict column
# meaning the reverse of a neighboring table's is one a reader misreads exactly
# once. This table scores mechanisms. Deleting an assertion on a healthy tree can
# never redden anything, so that move is not used below.
#
# CAUGHT is split further. A red that comes from the fixture guard
# ("fixture is wrong: ...") is NOT coverage of the mechanism -- it means the
# mutation broke the test's own setup before reaching the thing under test. A
# panic IS coverage when its top in-repo frame is non-test source, because a
# mutated bounds guard can only ever signal by panicking; it is not coverage when
# the panic is in the fixture. Filter panic frames by FULL PATH: runtime/panic.go
# ends in .go, is not _test.go, and a filename-only filter would invert the rule.
#
# Every mutation is an EXACT string replacement, not a regex. A regex that goes
# stale silently mutates nothing and scores a bogus UNNOTICED; the VOID row below
# exists because that happened while this script was being written. The
# replacement asserts its search string was present and unique.
#
# Run from anywhere. Requires a clean tree: each case restores with
# `git checkout --`, which would silently discard uncommitted work instead of the
# mutation and leave a clean tree doing it.
set -uo pipefail

cd "$(dirname "$0")" || exit 1
SRC=textutil.go

if [ -n "$(git status --porcelain -- "$SRC")" ]; then
	echo "REFUSING: $SRC has uncommitted changes. Commit before sabotaging --"
	echo "the per-case restore would throw your fix away, not the mutation."
	exit 1
fi

# textutil.go is a deliberately broken version of itself from the mutate below
# until the `git checkout --` that follows it, so every way out of that window has
# to restore -- including the ways this script does not choose to take. A killed
# run left the mutated file behind as ordinary-looking uncommitted work: a
# semantic edit to a tracked source file, which `git status` reports the same way
# it reports real work in progress, and which this box's standing rule tells the
# next agent not to throw away. The refusal at the top of this script then blocks
# the next run, because of the mess the last one made.
#
# Measured by kill on this scorer before this trap existed: SIGTERM and SIGHUP
# each left textutil.go mutated. Those are exactly what a wall-clock cap, systemd
# and a process-group kill send at an unattended run -- so the gap was not a
# corner case, it was the normal way this script gets stopped.
#
# The handler restores, clears the trap and re-raises, so the process dies BY the
# signal (rc 128+signum). A handler that restores and exits 0 tells every caller a
# killed run succeeded.
#
# SIGKILL cannot be caught by the process that receives it. It is the one gap left
# here, and it is named rather than papered over.
on_signal() { # <signal name>
	git checkout -- "$SRC"
	trap - "$1"
	kill -s "$1" $$
}

for sig in INT TERM HUP; do
	# shellcheck disable=SC2064  # expand $sig now: the handler must name its own signal
	trap "on_signal $sig" "$sig"
done

pass=0
fail=0

mutate() { # <find> <replace>; fails unless <find> occurs exactly once
	python3 - "$SRC" "$1" "$2" <<-'PY'
		import sys
		path, find, repl = sys.argv[1], sys.argv[2], sys.argv[3]
		src = open(path).read()
		n = src.count(find)
		if n != 1:
		    sys.stderr.write("search string occurs %d times, want exactly 1\n" % n)
		    sys.exit(1)
		open(path, "w").write(src.replace(find, repl))
	PY
}

classify() { # <exit_code> <output>
	local code="$1" out="$2"
	if [ "$code" -eq 0 ]; then echo "UNNOTICED"; return; fi
	if grep -q "fixture is wrong" <<<"$out"; then
		echo "CAUGHT (guard only -- NOT coverage)"
		return
	fi
	if grep -q "^panic:" <<<"$out"; then
		# The TOP in-repo frame decides, not the presence of any frame. A panic
		# raised inside the helper ALWAYS has the calling test frame below it in
		# the trace, so "contains no _test.go frame" can never be true and a
		# classifier written that way can never return the coverage branch. It
		# silently reports every panic as fixture damage. That was this script's
		# first version, and no row in the table panics, so the table scored a
		# clean 5/5 with the branch permanently dead.
		local top
		top="$(grep -oE '/internal/textutil/[a-z_]+\.go:[0-9]+' <<<"$out" | head -1)"
		if [ -n "$top" ] && [[ "$top" != *_test.go:* ]]; then
			echo "CAUGHT (panic in source -- coverage)"
		else
			echo "CAUGHT (panic in fixture -- NOT coverage)"
		fi
		return
	fi
	echo "CAUGHT (assertion fired)"
}

run_case() { # <name> <expect> <find> <replace>
	local name="$1" expect="$2"
	if ! mutate "$3" "$4" 2>/tmp/mutate.err; then
		printf '%-36s VOID -- %s' "$name" "$(cat /tmp/mutate.err)"
		fail=$((fail + 1))
		git checkout -- "$SRC"
		return
	fi
	if git diff --quiet -- "$SRC"; then
		printf '%-36s VOID -- mutation changed nothing\n' "$name"
		fail=$((fail + 1))
		git checkout -- "$SRC"
		return
	fi
	local out code verdict ok
	out="$(go test -count=1 . 2>&1)"
	code=$?
	verdict="$(classify "$code" "$out")"
	ok="  "
	case "$expect" in
	CAUGHT) [[ "$verdict" == CAUGHT*coverage* || "$verdict" == "CAUGHT (assertion fired)" ]] && ok="OK" ;;
	UNNOTICED) [ "$verdict" = "UNNOTICED" ] && ok="OK" ;;
	esac
	[ "$ok" = "OK" ] && pass=$((pass + 1)) || fail=$((fail + 1))
	printf '%-36s expect %-9s -> %-38s %s\n' "$name" "$expect" "$verdict" "$ok"
	git checkout -- "$SRC"
}

echo "=== mechanisms in the helper ==="

# The known-positive. Until this row is CAUGHT, every negative below is worthless.
run_case "helper reverts to a byte cut" CAUGHT \
	'	return string(unicode.ToUpper(first)) + s[width:]' \
	'	return strings.ToUpper(s[:1]) + s[1:]'

run_case "ToUpper dropped, rune kept whole" CAUGHT \
	'string(unicode.ToUpper(first))' \
	'string(first)'

run_case "invalid-lead-byte guard removed" CAUGHT \
	'if first == utf8.RuneError && width <= 1 {' \
	'if false && first == utf8.RuneError && width <= 1 {'

echo
echo "=== controls: rows that MUST come back UNNOTICED ==="

# Known-negative 1. Widening a guard past any value this package can see is a
# behavioral no-op. A scorer reporting CAUGHT here reports CAUGHT for
# everything, and its other rows mean nothing.
run_case "no-op: width<=1 becomes width<2" UNNOTICED \
	'width <= 1 {' \
	'width < 2 {'

# Known-negative 2, and it is a MEASUREMENT, not a prediction. The empty-string
# guard looks like the thing standing between this package and the old code's
# "slice bounds out of range" panic. It is not: utf8.DecodeRuneInString("")
# returns (RuneError, 0), so the invalid-lead-byte guard below already returns ""
# unchanged. Removing the early return therefore changes nothing.
#
# The guard is kept anyway -- it states the empty case at the top of the function
# instead of resting on a non-obvious stdlib return value -- but it is documented
# as belt-and-braces rather than load-bearing, because this row says so.
run_case "empty-string guard removed" UNNOTICED \
	'	if s == "" {' \
	'	if false {'

echo
echo "=== the gap this script used to report is now closed ==="
echo "cmd/seed and cmd/migrate-inber call sites: SCORED, in cmd/sabotage.sh."
echo "  This section used to read 'NOT SCORED ... reverting either call site to"
echo "  the byte cut reddens nothing', which was true when written and is not"
echo "  now. Both call sites have tests and both revert-to-byte-cut rows come"
echo "  back CAUGHT. Run the two scripts together: this one scores the helper,"
echo "  that one scores the callers."
echo
echo "  One correction worth carrying over. This script's claim that the old"
echo "  spelling is caught by the compiler 'needing the strings import back' is"
echo "  backwards as a safety net: at a call site it orphans the textutil import"
echo "  instead, and go test reports that as a non-zero exit that looks exactly"
echo "  like a failing assertion. cmd/sabotage.sh classifies a build failure as"
echo "  VOID for that reason. A compiler error is not coverage."

echo
echo "scored: $pass ok, $fail not ok"
[ "$fail" -eq 0 ] || exit 1
